package server

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

const negativeCacheMaxEntries = 10_000

func (s *Server) negativeLookup(key string) (int, http.Header, bool) {
	s.negativeMu.Lock()
	defer s.negativeMu.Unlock()
	entry, ok := s.negative[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(s.negative, key)
		}
		return 0, nil, false
	}
	return entry.status, entry.headers.Clone(), true
}

func (s *Server) negativeStore(key string, status int, headers http.Header) {
	s.negativeMu.Lock()
	defer s.negativeMu.Unlock()
	if s.negative == nil {
		s.negative = make(map[string]negativeCacheEntry)
	}
	now := time.Now()
	for existing, entry := range s.negative {
		if !now.Before(entry.expires) {
			delete(s.negative, existing)
		}
	}
	if _, exists := s.negative[key]; !exists && len(s.negative) >= negativeCacheMaxEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for existing, entry := range s.negative {
			if oldestKey == "" || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = existing, entry.expires
			}
		}
		delete(s.negative, oldestKey)
	}
	selected := make(http.Header)
	for _, name := range []string{"X-Error-Code", "X-Error-Message", "X-Repo-Commit", "Retry-After"} {
		if value := headers.Get(name); value != "" {
			selected.Set(name, value)
		}
	}
	s.negative[key] = negativeCacheEntry{expires: now.Add(s.currentConfig().CacheNegativeTTL), status: status, headers: selected}
}

func (s *Server) joinCacheMiss(key string) (bool, <-chan struct{}) {
	s.missMu.Lock()
	defer s.missMu.Unlock()
	if s.misses == nil {
		s.misses = make(map[string]*cacheMiss)
	}
	if flight, ok := s.misses[key]; ok {
		return false, flight.done
	}
	s.misses[key] = &cacheMiss{done: make(chan struct{})}
	return true, s.misses[key].done
}

func (s *Server) finishCacheMiss(key string) {
	s.missMu.Lock()
	defer s.missMu.Unlock()
	if flight, ok := s.misses[key]; ok {
		delete(s.misses, key)
		close(flight.done)
	}
}

func (s *Server) startOrphanedBlobGC(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = s.collectOrphanedBlobs(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Server) collectOrphanedBlobs(ctx context.Context) error {
	objects := s.currentStorage()
	if objects == nil || s.currentConfig().CacheGCGrace <= 0 {
		return nil
	}
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id FROM blobs b
		WHERE b.orphaned_at IS NOT NULL AND b.orphaned_at < now()-$1::interval
		AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id)`, s.currentConfig().CacheGCGrace.String())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		tx, err := s.db.SQL.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var key string
		if err = tx.QueryRowContext(ctx, `SELECT storage_key FROM blobs WHERE id=$1 AND orphaned_at IS NOT NULL AND orphaned_at < now()-$2::interval FOR UPDATE`, id, s.currentConfig().CacheGCGrace.String()).Scan(&key); err == sql.ErrNoRows {
			_ = tx.Rollback()
			continue
		} else if err != nil {
			_ = tx.Rollback()
			return err
		}
		var refs int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, id).Scan(&refs); err != nil {
			_ = tx.Rollback()
			return err
		}
		if refs > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, id)
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if err != nil {
				return err
			}
			continue
		}
		if err = objects.Delete(ctx, key); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM blobs WHERE id=$1`, id); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
