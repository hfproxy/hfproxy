package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BlobTotals counts every blob row and sums their sizes.
func (s *Store) BlobTotals(ctx context.Context) (count, size int64, err error) {
	err = s.SQL.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(size_bytes),0) FROM blobs`).Scan(&count, &size)
	return count, size, err
}

// LiveBytes is the size of the cached blobs that eviction may reclaim: the
// blobs of files cached from the Hub (not yet marked orphaned) that no
// published repository uses. Published content is never evicted, so it does not
// count against the cache size limit.
func (s *Store) LiveBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(b.size_bytes),0) FROM blobs b WHERE b.orphaned_at IS NULL
		AND EXISTS (SELECT 1 FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE a.blob_id=b.id AND r.origin='hub')
		AND NOT EXISTS (SELECT 1 FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE a.blob_id=b.id AND r.origin='local')`).Scan(&total)
	return total, err
}

// PinnedBytes is the size of the live blobs that eviction must not touch.
func (s *Store) PinnedBytes(ctx context.Context) (int64, error) {
	var bytes int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(b.size_bytes),0) FROM blobs b
		WHERE b.orphaned_at IS NULL AND EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND EXISTS (`+PinnedArtifactSQL+`))`).Scan(&bytes)
	return bytes, err
}

// EvictionCandidate is an unpinned cached file. Heat is its use count, faded
// to now with the policy's half-life.
type EvictionCandidate struct {
	ArtifactID, BlobID string
	Heat               float64
}

// EvictionPolicy says in which order files leave the cache.
type EvictionPolicy struct {
	Strategy      string // a config.Evict* name; anything else is LRU
	HalfLife      time.Duration
	CostWeight    float64
	CostBandwidth int64
	CostLatency   float64 // seconds
}

// defaultHeatHalfLife fades the use counts of a policy that names none.
const defaultHeatHalfLife = 7 * 24 * time.Hour

// EvictionScope narrows the candidates; the zero value is the whole cache.
type EvictionScope struct {
	RepositoryID string
	Namespace    string
	IdleFor      time.Duration
}

// order returns the ORDER BY expression that puts the first file to go first,
// adding its parameters to args. Scores are compared in log space, which keeps
// them finite for a file idle for years. What a file costs to fetch again is
// what its last fill took, or an estimate from its size when that is unknown
// (the fill found the blob already stored, was resumed, or predates the column).
func (p EvictionPolicy) order(args *[]any) string {
	bind := func(value float64) string {
		*args = append(*args, value)
		return fmt.Sprintf("$%d::double precision", len(*args))
	}
	logHeat := func() string {
		return `ln(GREATEST(a.heat,1e-300))-EXTRACT(EPOCH FROM now()-a.heat_at)::double precision*ln(2)/` + bind(p.HalfLife.Seconds())
	}
	logCost := func() string {
		return `ln(GREATEST(COALESCE(a.fill_seconds,b.size_bytes/` + bind(float64(p.CostBandwidth)) + `+` + bind(p.CostLatency) + `),0.001))`
	}
	switch p.Strategy {
	case "fifo":
		return `a.cached_at`
	case "lfu":
		return logHeat()
	case "gdsf":
		return logHeat() + `+` + logCost() + `-ln(GREATEST(b.size_bytes,1)::double precision)`
	case "cost_lru":
		return bind(p.CostWeight) + `*` + logCost() + `-ln(GREATEST(EXTRACT(EPOCH FROM now()-a.last_accessed_at)::double precision,0)+1)`
	default:
		return `a.last_accessed_at`
	}
}

// EvictionCandidates returns up to limit unpinned artifacts of the scope, in the
// order the policy evicts them.
func (s *Store) EvictionCandidates(ctx context.Context, policy EvictionPolicy, scope EvictionScope, limit int) ([]EvictionCandidate, error) {
	if policy.HalfLife <= 0 {
		policy.HalfLife = defaultHeatHalfLife
	}
	args := []any{limit}
	filter := ""
	if scope.RepositoryID != "" {
		args = append(args, scope.RepositoryID)
		filter += fmt.Sprintf(` AND r.id=$%d`, len(args))
	}
	if scope.Namespace != "" {
		args = append(args, scope.Namespace)
		filter += fmt.Sprintf(` AND `+namespaceOfRepository+`=$%d`, len(args))
	}
	if scope.IdleFor > 0 {
		args = append(args, scope.IdleFor.Seconds())
		filter += fmt.Sprintf(` AND a.last_accessed_at < now()-make_interval(secs => $%d::double precision)`, len(args))
	}
	order := policy.order(&args)
	args = append(args, policy.HalfLife.Seconds())
	heat := fmt.Sprintf(`a.heat*power(0.5,LEAST(EXTRACT(EPOCH FROM now()-a.heat_at)::double precision/$%d::double precision,1000))`, len(args))
	rows, err := s.SQL.QueryContext(ctx, `SELECT a.id,a.blob_id,`+heat+` FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id
		WHERE r.origin='hub' AND NOT EXISTS (`+PinnedArtifactSQL+`)`+filter+`
		ORDER BY `+order+`, a.id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	return scanEvictionCandidates(rows)
}

// StaleRevisionCandidates returns up to limit unpinned artifacts of the commits
// beyond the keep most recently used of each repository.
func (s *Store) StaleRevisionCandidates(ctx context.Context, keep int64, limit int) ([]EvictionCandidate, error) {
	rows, err := s.SQL.QueryContext(ctx, `WITH ranked AS (
			SELECT a.repository_id,a.commit_sha,row_number() OVER (PARTITION BY a.repository_id ORDER BY MAX(a.last_accessed_at) DESC,a.commit_sha) AS position
			FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE r.origin='hub' GROUP BY a.repository_id,a.commit_sha
		)
		SELECT a.id,a.blob_id,a.heat FROM artifacts a JOIN ranked k ON k.repository_id=a.repository_id AND k.commit_sha=a.commit_sha
		WHERE k.position>$2 AND NOT EXISTS (`+PinnedArtifactSQL+`)
		ORDER BY a.last_accessed_at, a.id LIMIT $1`, limit, keep)
	if err != nil {
		return nil, err
	}
	return scanEvictionCandidates(rows)
}

// namespaceOfRepository is the user or organisation of a repository; a
// repository without one (gpt2) is its own namespace.
const namespaceOfRepository = `split_part(r.repo_id,'/',1)`

// RepositoriesOver lists the repositories whose cached files exceed limit bytes.
func (s *Store) RepositoriesOver(ctx context.Context, limit int64) ([]string, error) {
	return s.scopesOver(ctx, `r.id`, limit)
}

// NamespacesOver lists the namespaces whose cached files exceed limit bytes.
func (s *Store) NamespacesOver(ctx context.Context, limit int64) ([]string, error) {
	return s.scopesOver(ctx, namespaceOfRepository, limit)
}

func (s *Store) scopesOver(ctx context.Context, scope string, limit int64) ([]string, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT scope FROM (
			SELECT DISTINCT `+scope+` AS scope,b.id,b.size_bytes FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE r.origin='hub'
		) files GROUP BY scope HAVING SUM(size_bytes)>$1 ORDER BY scope`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// ScopeBytes is the size of the distinct blobs cached for a repository or a
// namespace. A blob shared by two commits of a repository counts once.
func (s *Store) ScopeBytes(ctx context.Context, scope EvictionScope) (int64, error) {
	filter, value := `r.id=$1`, scope.RepositoryID
	if scope.Namespace != "" {
		filter, value = namespaceOfRepository+`=$1`, scope.Namespace
	}
	var total int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM (
		SELECT DISTINCT b.id,b.size_bytes FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE r.origin='hub' AND `+filter+`) files`, value).Scan(&total)
	return total, err
}

// RecentRequests counts the requests for a file within window, whatever their
// outcome: the download log is the ghost list of the admission policies.
func (s *Store) RecentRequests(ctx context.Context, repoID, commit, path string, window time.Duration) (int64, error) {
	var count int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE repository_id=$1 AND commit_sha=$2 AND path=$3 AND started_at > now()-make_interval(secs => $4::double precision)`,
		repoID, commit, path, window.Seconds()).Scan(&count)
	return count, err
}

// CachedFiles counts the files cached from the Hub.
func (s *Store) CachedFiles(ctx context.Context) (int64, error) {
	var total int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE r.origin='hub'`).Scan(&total)
	return total, err
}

func scanEvictionCandidates(rows *sql.Rows) ([]EvictionCandidate, error) {
	defer rows.Close()
	var batch []EvictionCandidate
	for rows.Next() {
		var c EvictionCandidate
		if err := rows.Scan(&c.ArtifactID, &c.BlobID, &c.Heat); err != nil {
			return nil, err
		}
		batch = append(batch, c)
	}
	return batch, rows.Err()
}

// EvictArtifact removes an artifact and, when no other artifact uses its blob,
// marks the blob orphaned for the grace-period collector. It returns the bytes
// freed (0 when the blob is still referenced).
func (s *Store) EvictArtifact(ctx context.Context, artifactID, blobID string) (int64, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Lock the blob first, the same order publication uses, so a concurrent
	// fill cannot attach a new artifact between the count and the orphan mark.
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM blobs WHERE id=$1 FOR UPDATE`, blobID).Scan(&size); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, artifactID); err != nil {
		return 0, err
	}
	var refs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, blobID).Scan(&refs); err != nil {
		return 0, err
	}
	var freed int64
	if refs == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1 AND orphaned_at IS NULL`, blobID)
		if err != nil {
			return 0, err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			freed = size
		}
	}
	return freed, tx.Commit()
}

// OrphanedBlobIDs lists blobs that no artifact uses and that have been orphaned
// for longer than grace.
func (s *Store) OrphanedBlobIDs(ctx context.Context, grace time.Duration) ([]string, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT id FROM blobs b
		WHERE b.orphaned_at IS NOT NULL AND b.orphaned_at < now()-$1::interval
		AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id)`, grace.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CollectOrphanedBlob deletes one orphaned blob: under a row lock it re-checks
// that the blob is still orphaned past grace and unreferenced, calls
// deleteObject with its storage key, then removes the row. A blob that gained a
// reference meanwhile is un-orphaned instead. It reports whether the blob was
// deleted.
func (s *Store) CollectOrphanedBlob(ctx context.Context, id string, grace time.Duration, deleteObject func(ctx context.Context, storageKey string) error) (bool, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT storage_key FROM blobs WHERE id=$1 AND orphaned_at IS NOT NULL AND orphaned_at < now()-$2::interval FOR UPDATE`, id, grace.String()).Scan(&key); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var refs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, id).Scan(&refs); err != nil {
		return false, err
	}
	if refs > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err := deleteObject(ctx, key); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE id=$1`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RetentionJob names a periodic clean-up of old rows.
type RetentionJob string

const (
	RetainSessions           RetentionJob = "sessions"
	RetainLoginAttempts      RetentionJob = "login_attempts"
	RetainDownloads          RetentionJob = "downloads"
	RetainAbandonedDownloads RetentionJob = "downloads_abandoned"
	RetainAudit              RetentionJob = "audit_events"
)

// RunRetention applies one batch of a retention job and returns how many rows it
// changed. olderThan is the age limit for the downloads and audit jobs.
func (s *Store) RunRetention(ctx context.Context, job RetentionJob, batch int, olderThan time.Duration) (int64, error) {
	var query string
	args := []any{batch}
	switch job {
	case RetainSessions:
		query = `DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at < now() LIMIT $1)`
	case RetainLoginAttempts:
		query = `DELETE FROM login_attempts WHERE key IN (SELECT key FROM login_attempts WHERE window_start < now()-interval '1 hour' LIMIT $1)`
	case RetainAbandonedDownloads:
		// A download whose process died, or whose bookkeeping was lost, would stay
		// "in progress" forever and inflate the console's counter.
		query = `UPDATE downloads SET status='failed',error_class='abandoned',completed_at=now() WHERE id IN (SELECT id FROM downloads WHERE status='downloading' AND started_at < now()-interval '12 hours' LIMIT $1)`
	case RetainDownloads:
		query = `DELETE FROM downloads WHERE id IN (SELECT id FROM downloads WHERE started_at < now()-make_interval(secs => $2::double precision) LIMIT $1)`
		args = append(args, olderThan.Seconds())
	case RetainAudit:
		query = `DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE created_at < now()-make_interval(secs => $2::double precision) LIMIT $1)`
		args = append(args, olderThan.Seconds())
	default:
		return 0, fmt.Errorf("unknown retention job %q", job)
	}
	result, err := s.SQL.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
