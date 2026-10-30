package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Resumption is the saved state of a chunked download: how many bytes of an
// upstream file are already stored as blocks in the object store.
type Resumption struct {
	ID             string
	RepositoryID   string
	CommitSHA      string
	Path           string
	ETag           string
	TotalSize      int64
	ContentType    string
	ChunkSize      int64
	CompletedBytes int64
	// ResumedBytes is what an earlier attempt had already stored; it is not saved.
	ResumedBytes int64
}

const resumptionColumns = `id,repository_id,commit_sha,path,upstream_etag,total_size,content_type,chunk_size,completed_bytes`

type rowScanner interface{ Scan(dest ...any) error }

func scanResumption(row rowScanner) (*Resumption, error) {
	r := &Resumption{}
	err := row.Scan(&r.ID, &r.RepositoryID, &r.CommitSHA, &r.Path, &r.ETag, &r.TotalSize, &r.ContentType, &r.ChunkSize, &r.CompletedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// resumeLockKey names the advisory lock of one file; fills and the cleanup of
// stale resumptions must use the same one.
func resumeLockKey(repositoryID, commit, filePath string) string {
	return repositoryID + "|" + commit + "|" + filePath
}

// LockResume takes the cluster-wide lock that serialises every fill of one
// file, whichever replica runs it. The returned function releases it; closing
// the session releases it too if the explicit unlock fails.
func (s *Store) LockResume(ctx context.Context, repositoryID, commit, filePath string) (func(), error) {
	conn, err := s.SQL.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire download resume connection: %w", err)
	}
	lockKey := resumeLockKey(repositoryID, commit, filePath)
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("lock download resume: %w", err)
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey)
		_ = conn.Close()
	}, nil
}

// SetDownloadBytes records the progress of a running download.
func (s *Store) SetDownloadBytes(ctx context.Context, downloadID string, bytes int64) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE downloads SET bytes=$2 WHERE id=$1 AND status='downloading'`, downloadID, bytes)
	return err
}

// FindResumption returns the saved state of a file, or ErrNotFound.
func (s *Store) FindResumption(ctx context.Context, repositoryID, commit, filePath string) (*Resumption, error) {
	return scanResumption(s.SQL.QueryRowContext(ctx, `SELECT `+resumptionColumns+`
		FROM download_resumptions WHERE repository_id=$1 AND commit_sha=$2 AND path=$3`, repositoryID, commit, filePath))
}

// CreateResumption stores the state of a download that has no block yet.
func (s *Store) CreateResumption(ctx context.Context, r *Resumption) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO download_resumptions(`+resumptionColumns+`)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,0)`, r.ID, r.RepositoryID, r.CommitSHA, r.Path, r.ETag, r.TotalSize, r.ContentType, r.ChunkSize)
	return err
}

// ResetResumption restarts a download from the first byte with new metadata.
func (s *Store) ResetResumption(ctx context.Context, id, etag string, total int64, contentType string, chunkSize int64) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE download_resumptions SET upstream_etag=$2,total_size=$3,content_type=$4,chunk_size=$5,completed_bytes=0,updated_at=now() WHERE id=$1`,
		id, etag, total, contentType, chunkSize)
	return err
}

// SetResumptionETag saves the validator the Hub returned for a download.
func (s *Store) SetResumptionETag(ctx context.Context, id, etag string) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE download_resumptions SET upstream_etag=$2,updated_at=now() WHERE id=$1`, id, etag)
	return err
}

// AdvanceResumption moves the completed byte count from `from` to `to`. It
// returns false when another writer changed the count in the meantime.
func (s *Store) AdvanceResumption(ctx context.Context, id string, from, to int64) (bool, error) {
	result, err := s.SQL.ExecContext(ctx, `UPDATE download_resumptions SET completed_bytes=$2,updated_at=now() WHERE id=$1 AND completed_bytes=$3`, id, to, from)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	return changed == 1, nil
}

// DeleteResumption forgets the state of a finished or abandoned download.
func (s *Store) DeleteResumption(ctx context.Context, id string) error {
	_, err := s.SQL.ExecContext(ctx, `DELETE FROM download_resumptions WHERE id=$1`, id)
	return err
}

// StaleResumptions lists downloads that were not touched for maxAge.
func (s *Store) StaleResumptions(ctx context.Context, maxAge time.Duration, limit int) ([]*Resumption, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT `+resumptionColumns+`
		FROM download_resumptions WHERE updated_at < now()-($1::double precision * interval '1 hour') ORDER BY updated_at LIMIT $2`, maxAge.Hours(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []*Resumption
	for rows.Next() {
		item, err := scanResumption(rows)
		if err != nil {
			return nil, err
		}
		stale = append(stale, item)
	}
	return stale, rows.Err()
}

// CleanupStaleResumption removes a stale download once nobody is filling the
// file. discard deletes its blocks first; if it fails the state is kept so the
// next pass retries. It reports whether the state was removed.
func (s *Store) CleanupStaleResumption(ctx context.Context, candidate *Resumption, maxAge time.Duration, discard func(context.Context, *Resumption) error) (bool, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// The same key as LockResume, so a fill in progress keeps the cleanup away.
	lockKey := resumeLockKey(candidate.RepositoryID, candidate.CommitSHA, candidate.Path)
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, lockKey).Scan(&locked); err != nil || !locked {
		return false, err
	}
	current, err := scanResumption(tx.QueryRowContext(ctx, `SELECT `+resumptionColumns+`
		FROM download_resumptions WHERE id=$1 AND updated_at < now()-($2::double precision * interval '1 hour')`, candidate.ID, maxAge.Hours()))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := discard(ctx, current); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM download_resumptions WHERE id=$1`, current.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
