package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Artifact is a cached file together with the blob that holds its bytes.
type Artifact struct {
	ID          string
	Repository  string
	Commit      string
	Path        string
	BlobID      string
	StorageKey  string
	SHA256      string
	Size        int64
	ETag        string
	ContentType string
	LastMod     sql.NullTime
	// ObjectStored says that this fill uploaded the object itself. When it did
	// not (it saw the blob already recorded), the object is checked before a new
	// record is trusted.
	ObjectStored bool
}

// Caller identifies who triggered a download, for the download log. Empty
// fields are stored as NULL.
type Caller struct {
	UserID   string
	TokenID  string
	ClientIP string
}

// FindArtifact returns the cached file for a commit and path, or sql.ErrNoRows.
func (s *Store) FindArtifact(ctx context.Context, repoID, commit, file string) (Artifact, error) {
	var a Artifact
	err := s.SQL.QueryRowContext(ctx, `SELECT a.id,a.repository_id,a.commit_sha,a.path,a.blob_id,b.storage_key,b.sha256,b.size_bytes,a.etag,b.content_type,a.last_modified FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.commit_sha=$2 AND a.path=$3`, repoID, commit, file).
		Scan(&a.ID, &a.Repository, &a.Commit, &a.Path, &a.BlobID, &a.StorageKey, &a.SHA256, &a.Size, &a.ETag, &a.ContentType, &a.LastMod)
	return a, err
}

// TouchArtifact records an access for eviction. It writes at most once a minute
// per artifact. Only a download counts towards the file's frequency: clients
// send a HEAD before every load, even of a file they already hold, and the
// download that follows it within the minute must still be counted. The minute
// also makes one client's burst of Range requests a single use.
func (s *Store) TouchArtifact(ctx context.Context, id string, download bool, halfLife time.Duration) error {
	if !download {
		_, err := s.SQL.ExecContext(ctx, `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1 AND last_accessed_at < now()-interval '1 minute'`, id)
		return err
	}
	// A new artifact starts at heat 1 as of its caching, so a download within
	// the first minute adds nothing. The exponent is bounded because power()
	// fails on an underflow.
	_, err := s.SQL.ExecContext(ctx, `UPDATE artifacts SET last_accessed_at=now(),heat_at=now(),
		heat=heat*power(0.5,LEAST(EXTRACT(EPOCH FROM now()-heat_at)::double precision/$2::double precision,1000))+1
		WHERE id=$1 AND heat_at < now()-interval '1 minute'`, id, halfLife.Seconds())
	return err
}

// FreshRevision returns the commit a branch or tag resolved to within maxAge,
// or "" when there is none.
func (s *Store) FreshRevision(ctx context.Context, repoID, ref string, maxAge time.Duration) (string, error) {
	var commit string
	err := s.SQL.QueryRowContext(ctx, `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2 AND resolved_at>now()-($3 * interval '1 second')`, repoID, ref, maxAge.Seconds()).Scan(&commit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return commit, err
}

// LastKnownRevision returns the commit a branch or tag last resolved to,
// however old, or "".
func (s *Store) LastKnownRevision(ctx context.Context, repoID, ref string) (string, error) {
	var commit string
	err := s.SQL.QueryRowContext(ctx, `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2`, repoID, ref).Scan(&commit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return commit, err
}

// SaveRevision remembers which commit a branch or tag resolved to.
func (s *Store) SaveRevision(ctx context.Context, repoID, ref, commit string) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO revisions(id,repository_id,requested_ref,commit_sha,resolved_at) VALUES($1,$2,$3,$4,now())
		ON CONFLICT(repository_id,requested_ref) DO UPDATE SET commit_sha=EXCLUDED.commit_sha,resolved_at=now()`, NewID(), repoID, ref, commit)
	return err
}

// StartDownload logs a download that is in progress.
func (s *Store) StartDownload(ctx context.Context, id, repoID, commit, path string, caller Caller) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO downloads(id,repository_id,commit_sha,path,status,bytes,user_id,token_id,client_ip) VALUES($1,$2,$3,$4,'downloading',0,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''))`, id, repoID, commit, path, caller.UserID, caller.TokenID, caller.ClientIP)
	return err
}

// FinishDownload closes a download started with StartDownload.
func (s *Store) FinishDownload(ctx context.Context, id string, bytes int64, errorClass string) error {
	status := "miss"
	if errorClass != "" {
		status = "failed"
	}
	// A fill that succeeded is a miss; it is counted for its repository in the
	// same statement. Closing a download twice counts it once.
	_, err := s.SQL.ExecContext(ctx, `WITH closed AS (
		UPDATE downloads SET status=$2,completed_at=now(),bytes=$3,error_class=$4 WHERE id=$1 AND status='downloading' RETURNING repository_id,status
	)
	INSERT INTO repository_download_counts(repository_id,hits,misses)
	SELECT repository_id,0,1 FROM closed WHERE status='miss'
	ON CONFLICT (repository_id) DO UPDATE SET misses=repository_download_counts.misses+1`, id, status, bytes, errorClass)
	return err
}

// BlobExists reports whether a blob with this SHA-256 is already recorded.
func (s *Store) BlobExists(ctx context.Context, sha256 string) (bool, error) {
	var exists bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256=$1)`, sha256).Scan(&exists)
	return exists, err
}

// PublishInput describes a file that has been downloaded and stored.
type PublishInput struct {
	RepoID      string
	Commit      string
	Path        string
	SHA256      string
	Size        int64
	StorageKey  string
	ContentType string
	ETag        string
	LastMod     sql.NullTime
	// ObjectStored says that this fill uploaded the object itself. When it did
	// not (it saw the blob already recorded), the object is checked before a new
	// record is trusted.
	ObjectStored bool
	// FillSeconds is how long fetching the whole file took, 0 when unknown.
	FillSeconds float64
}

// PublishError says which step of PublishArtifact failed.
type PublishError struct {
	Stage string // begin, read_blob, insert_blob, restore_blob, insert_artifact, commit
	Err   error
}

func (e *PublishError) Error() string {
	return fmt.Sprintf("publish artifact (%s): %v", e.Stage, e.Err)
}
func (e *PublishError) Unwrap() error { return e.Err }

// PublishArtifact records the blob and the artifact that points to it in one
// transaction. ensureObject is called with the storage key so the caller can
// check or restore the object when the blob was already known, or when the
// caller did not upload the object itself (see PublishInput.ObjectStored); its
// error is returned as is (not wrapped in a PublishError).
func (s *Store) PublishArtifact(ctx context.Context, in PublishInput, ensureObject func(ctx context.Context, storageKey string) error) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return &PublishError{"begin", err}
	}
	defer tx.Rollback()
	var blobID, existingKey string
	err = tx.QueryRowContext(ctx, `SELECT id,storage_key FROM blobs WHERE sha256=$1 FOR UPDATE`, in.SHA256).Scan(&blobID, &existingKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO blobs(id,sha256,size_bytes,storage_key,content_type,verified_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(sha256) DO UPDATE SET orphaned_at=NULL RETURNING id,storage_key`, NewID(), in.SHA256, in.Size, in.StorageKey, in.ContentType).Scan(&blobID, &existingKey)
		if err != nil {
			return &PublishError{"insert_blob", err}
		}
		// No row was found, so the caller's idea of the blob may be stale: it may
		// have seen the row, skipped the upload, and then waited on the collector,
		// which deleted the object and the row under the lock this lookup queued
		// behind. A fill that uploaded the object itself has nothing to check; any
		// other must make sure the object is there before the catalog points at it.
		if !in.ObjectStored || existingKey != in.StorageKey {
			if err := ensureObject(ctx, existingKey); err != nil {
				return err
			}
		}
	case err != nil:
		return &PublishError{"read_blob", err}
	default:
		if err := ensureObject(ctx, existingKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, blobID); err != nil {
			return &PublishError{"restore_blob", err}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,etag,last_modified,upstream_size,fill_seconds) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9::double precision,0))
		ON CONFLICT(repository_id,commit_sha,path) DO UPDATE SET blob_id=EXCLUDED.blob_id,etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,upstream_size=EXCLUDED.upstream_size,cached_at=now(),last_accessed_at=now(),fill_seconds=COALESCE(EXCLUDED.fill_seconds,artifacts.fill_seconds)`,
		NewID(), in.RepoID, in.Commit, in.Path, blobID, in.ETag, nullTime(in.LastMod), in.Size, in.FillSeconds); err != nil {
		return &PublishError{"insert_artifact", err}
	}
	if err := tx.Commit(); err != nil {
		return &PublishError{"commit", err}
	}
	return nil
}

func nullTime(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time
}

// DownloadEvent is one finished download to log.
type DownloadEvent struct {
	RepoID, Commit, Path, Status, ErrorClass string
	Bytes                                    int64
	Caller                                   Caller
}

// RecordDownloads logs several finished downloads with a single statement.
func (s *Store) RecordDownloads(ctx context.Context, events []DownloadEvent) error {
	if len(events) == 0 {
		return nil
	}
	const columns = 10
	var query strings.Builder
	query.WriteString("WITH inserted AS (INSERT INTO downloads(id,repository_id,commit_sha,path,status,completed_at,bytes,error_class,user_id,token_id,client_ip) VALUES ")
	args := make([]any, 0, len(events)*columns)
	for i, event := range events {
		if i > 0 {
			query.WriteString(",")
		}
		n := i*columns + 1
		fmt.Fprintf(&query, "($%d,$%d,$%d,$%d,$%d,now(),$%d,$%d,NULLIF($%d,''),NULLIF($%d,''),NULLIF($%d,''))", n, n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9)
		args = append(args, NewID(), event.RepoID, event.Commit, event.Path, event.Status, event.Bytes, event.ErrorClass, event.Caller.UserID, event.Caller.TokenID, event.Caller.ClientIP)
	}
	// Hits and misses are counted per repository in the same statement. The
	// repositories are taken in a fixed order so that two batches updating
	// several of them cannot wait on each other.
	query.WriteString(` RETURNING repository_id,status)
		INSERT INTO repository_download_counts(repository_id,hits,misses)
		SELECT repository_id,COUNT(*) FILTER(WHERE status='hit'),COUNT(*) FILTER(WHERE status='miss') FROM inserted GROUP BY repository_id ORDER BY repository_id
		ON CONFLICT (repository_id) DO UPDATE SET hits=repository_download_counts.hits+EXCLUDED.hits,misses=repository_download_counts.misses+EXCLUDED.misses`)
	_, err := s.SQL.ExecContext(ctx, query.String(), args...)
	return err
}
