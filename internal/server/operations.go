package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

var (
	errFillBusy    = errors.New("cache fill capacity exhausted")
	errStagingFull = errors.New("insufficient staging space")

	// fillSlotWait is how long a cache fill waits for a free slot before the
	// client is told to retry.
	fillSlotWait = 15 * time.Second
)

const (
	defaultFillConcurrency = 4
	maintenanceInterval    = 10 * time.Minute
	retentionBatch         = 5000
	retentionMaxBatches    = 200
	evictionBatch          = 200
)

// acquireFillSlot bounds how many cache fills (and their staging files) run at
// once. A Server without slots, such as one built by tests, is unbounded.
func (s *Server) acquireFillSlot(ctx context.Context) (func(), error) {
	if s.fillSlots == nil {
		return func() {}, nil
	}
	timer := time.NewTimer(fillSlotWait)
	defer timer.Stop()
	select {
	case s.fillSlots <- struct{}{}:
		return func() { <-s.fillSlots }, nil
	case <-timer.C:
		return nil, errFillBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) stagingDirectory() string {
	if dir := s.currentConfig().StagingDir; dir != "" {
		return dir
	}
	return os.TempDir()
}

func (s *Server) createStagingFile(pattern string) (*os.File, error) {
	return os.CreateTemp(s.currentConfig().StagingDir, pattern)
}

// reserveStaging refuses a fill whose file would not fit next to the fills
// already running. The reservation counts the full size of every running fill
// even though part of it is already on disk, which errs on the safe side.
func (s *Server) reserveStaging(size int64) (func(), error) {
	reserved := s.stagingReserved.Add(size)
	release := func() {
		s.observeStaging(s.stagingReserved.Add(-size))
	}
	if free, ok := diskFree(s.stagingDirectory()); ok && uint64(reserved) > free {
		release()
		return nil, errStagingFull
	}
	s.observeStaging(reserved)
	return release, nil
}

// observeStaging publishes how much staging space is free and how much running
// fills have reserved.
func (s *Server) observeStaging(reserved int64) {
	if s.metrics == nil {
		return
	}
	s.metrics.setGauge("hfproxy_staging_reserved_bytes", float64(reserved))
	if free, ok := diskFree(s.stagingDirectory()); ok {
		s.metrics.setGauge("hfproxy_staging_free_bytes", float64(free))
	}
}

// stagingLow reports whether the staging directory has less free space than
// HFPROXY_STAGING_MIN_FREE_BYTES. It is false when the check is off or the free
// space cannot be read.
func (s *Server) stagingLow() bool {
	minimum := s.currentConfig().StagingMinFreeBytes
	if minimum <= 0 {
		return false
	}
	free, ok := diskFree(s.stagingDirectory())
	return ok && free < uint64(minimum)
}

func (s *Server) startMaintenance(ctx context.Context) {
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for tick := 0; ; {
			evictionOnly := false
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-s.maintenanceWake:
				evictionOnly = true
			}
			s.observeStaging(s.stagingReserved.Load())
			if evicted, err := s.evictOverQuota(ctx); err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("cache eviction failed", "class", fmt.Sprintf("%T", err))
				}
			} else if evicted > 0 {
				s.logger.Info("cache eviction completed", "artifacts", evicted)
			}
			if evictionOnly {
				continue // the periodic timer is still running
			}
			if tick%6 == 0 {
				s.runRetention(ctx)
			}
			tick++
			timer.Reset(maintenanceInterval)
		}
	}()
}

// runRetention removes expired sessions and, when configured, old download
// events and audit events, in bounded batches.
func (s *Server) runRetention(ctx context.Context) {
	cfg := s.currentConfig()
	type job struct {
		name      db.RetentionJob
		olderThan time.Duration
	}
	jobs := []job{{name: db.RetainSessions}, {name: db.RetainAbandonedDownloads}, {name: db.RetainLoginAttempts}}
	if cfg.RetentionDownloads > 0 {
		jobs = append(jobs, job{db.RetainDownloads, cfg.RetentionDownloads})
	}
	if cfg.RetentionAudit > 0 {
		jobs = append(jobs, job{db.RetainAudit, cfg.RetentionAudit})
	}
	for _, j := range jobs {
		deleted, err := s.retainInBatches(ctx, j.name, j.olderThan)
		if deleted > 0 {
			if s.metrics != nil {
				s.metrics.add("hfproxy_retention_deleted_total", []string{"table=" + string(j.name)}, float64(deleted))
			}
			s.logger.Info("retention purge completed", "table", string(j.name), "deleted", deleted)
		}
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("retention purge failed", "table", string(j.name), "class", fmt.Sprintf("%T", err))
		}
	}
}

func (s *Server) retainInBatches(ctx context.Context, job db.RetentionJob, olderThan time.Duration) (int64, error) {
	var total int64
	for i := 0; i < retentionMaxBatches; i++ {
		n, err := s.db.RunRetention(ctx, job, retentionBatch, olderThan)
		total += n
		if err != nil {
			return total, err
		}
		if n < retentionBatch {
			return total, nil
		}
	}
	return total, nil
}

// evictOverQuota enforces the cache limits: the idle and revision limits name
// files outright, then the per-repository, per-namespace and total sizes each
// drop files in the order of the chosen strategy. Blobs left without artifacts
// are marked orphaned, and the existing garbage collector deletes their objects
// after the grace period.
func (s *Server) evictOverQuota(ctx context.Context) (int, error) {
	cfg := s.currentConfig()
	limits := cfg.CacheEviction.Normalized()
	policy := db.EvictionPolicy{Strategy: limits.Strategy, HalfLife: limits.HalfLife(), CostWeight: limits.Cost.Weight, CostBandwidth: limits.Cost.Bandwidth, CostLatency: limits.Cost.LatencySeconds}
	evicted := 0
	for _, step := range []func(context.Context, config.CacheEviction, db.EvictionPolicy) (int, error){
		s.evictIdle, s.evictStaleRevisions, s.evictOversizedScopes, s.evictRingFiles,
	} {
		n, err := step(ctx, limits, policy)
		evicted += n
		if err != nil {
			return evicted, err
		}
	}
	limit := cfg.CacheMaxTotalSize
	if ring := limits.FIFO.RingSize; limits.Strategy == config.EvictFIFO && ring > 0 && (limit == 0 || ring < limit) {
		limit = ring
	}
	if limit <= 0 {
		return evicted, nil
	}
	total, err := s.db.LiveBytes(ctx)
	if err != nil {
		return evicted, err
	}
	if total <= limit {
		return evicted, nil
	}
	target := evictionTarget(limit, limits)
	n, err := s.evictUntil(ctx, "total", func(ctx context.Context) ([]db.EvictionCandidate, error) {
		return s.db.EvictionCandidates(ctx, policy, db.EvictionScope{}, evictionBatch)
	}, func(_ context.Context, freed int64) (bool, error) {
		total -= freed
		return total <= target, nil
	})
	evicted += n
	if err != nil {
		return evicted, err
	}
	if total > limit {
		s.logger.Warn("the cache is over its size limit but what remains is pinned or still referenced", "limit_bytes", limit, "used_bytes", total)
	}
	return evicted, nil
}

func evictionTarget(limit int64, limits config.CacheEviction) int64 {
	return int64(float64(limit) * float64(limits.TargetPercent) / 100)
}

// evictUntil drops the files next returns, batch after batch, until done says
// enough, given the bytes each one freed, or nothing is left to drop.
func (s *Server) evictUntil(ctx context.Context, reason string, next func(context.Context) ([]db.EvictionCandidate, error), done func(context.Context, int64) (bool, error)) (int, error) {
	evicted := 0
	for {
		batch, err := next(ctx)
		if err != nil || len(batch) == 0 {
			return evicted, err
		}
		for _, c := range batch {
			freed, err := s.db.EvictArtifact(ctx, c.ArtifactID, c.BlobID)
			if err != nil {
				return evicted, err
			}
			evicted++
			if s.metrics != nil {
				s.metrics.add("hfproxy_cache_evictions_total", []string{"reason=" + reason}, 1)
			}
			stop, err := done(ctx, freed)
			if err != nil || stop {
				return evicted, err
			}
		}
	}
}

func neverDone(context.Context, int64) (bool, error) { return false, nil }

// evictIdle drops the files nobody asked for during MaxIdleHours.
func (s *Server) evictIdle(ctx context.Context, limits config.CacheEviction, policy db.EvictionPolicy) (int, error) {
	if limits.MaxIdleHours <= 0 {
		return 0, nil
	}
	scope := db.EvictionScope{IdleFor: time.Duration(limits.MaxIdleHours) * time.Hour}
	return s.evictUntil(ctx, "idle", func(ctx context.Context) ([]db.EvictionCandidate, error) {
		return s.db.EvictionCandidates(ctx, policy, scope, evictionBatch)
	}, neverDone)
}

// evictStaleRevisions keeps the MaxRevisions most recently used commits of each
// repository. A file a newer commit shares keeps its blob and only loses its
// entry for the old commit.
func (s *Server) evictStaleRevisions(ctx context.Context, limits config.CacheEviction, _ db.EvictionPolicy) (int, error) {
	if limits.MaxRevisions <= 0 {
		return 0, nil
	}
	return s.evictUntil(ctx, "revisions", func(ctx context.Context) ([]db.EvictionCandidate, error) {
		return s.db.StaleRevisionCandidates(ctx, limits.MaxRevisions, evictionBatch)
	}, neverDone)
}

// evictOversizedScopes brings each repository, then each namespace, back under
// its limit. Usage is read again after each file because a blob can be shared
// inside a scope, where dropping one of its entries frees nothing.
func (s *Server) evictOversizedScopes(ctx context.Context, limits config.CacheEviction, policy db.EvictionPolicy) (int, error) {
	evicted := 0
	for _, level := range []struct {
		reason string
		limit  int64
		over   func(context.Context, int64) ([]string, error)
		scope  func(string) db.EvictionScope
	}{
		{"repository", limits.MaxRepositorySize, s.db.RepositoriesOver, func(id string) db.EvictionScope { return db.EvictionScope{RepositoryID: id} }},
		{"namespace", limits.MaxNamespaceSize, s.db.NamespacesOver, func(name string) db.EvictionScope { return db.EvictionScope{Namespace: name} }},
	} {
		if level.limit <= 0 {
			continue
		}
		names, err := level.over(ctx, level.limit)
		if err != nil {
			return evicted, err
		}
		target := evictionTarget(level.limit, limits)
		for _, name := range names {
			scope := level.scope(name)
			n, err := s.evictUntil(ctx, level.reason, func(ctx context.Context) ([]db.EvictionCandidate, error) {
				return s.db.EvictionCandidates(ctx, policy, scope, evictionBatch)
			}, func(ctx context.Context, _ int64) (bool, error) {
				used, err := s.db.ScopeBytes(ctx, scope)
				return used <= target, err
			})
			evicted += n
			if err != nil {
				return evicted, err
			}
		}
	}
	return evicted, nil
}

// evictRingFiles holds the circular cache of the fifo strategy to its number of
// files.
func (s *Server) evictRingFiles(ctx context.Context, limits config.CacheEviction, policy db.EvictionPolicy) (int, error) {
	if limits.Strategy != config.EvictFIFO || limits.FIFO.RingFiles <= 0 {
		return 0, nil
	}
	files, err := s.db.CachedFiles(ctx)
	if err != nil || files <= limits.FIFO.RingFiles {
		return 0, err
	}
	return s.evictUntil(ctx, "ring", func(ctx context.Context) ([]db.EvictionCandidate, error) {
		return s.db.EvictionCandidates(ctx, policy, db.EvictionScope{}, evictionBatch)
	}, func(context.Context, int64) (bool, error) {
		files--
		return files <= limits.FIFO.RingFiles, nil
	})
}

// pinnedBytes is the size of the live blobs that eviction must not touch.
func (s *Server) pinnedBytes(ctx context.Context) (int64, error) {
	return s.db.PinnedBytes(ctx)
}

var (
	// apiReadTimeout bounds how long a client may take to send an API or
	// login request, body included; apiWriteTimeout bounds the response.
	apiReadTimeout  = 30 * time.Second
	apiWriteTimeout = 5 * time.Minute
)

// requestDeadlines gives every request except file downloads a read and write
// deadline, so a client dribbling a request body cannot hold a connection.
// Downloads stream for as long as the transfer makes progress and are bound by
// the idle timeouts on the upstream and storage sides instead.
func (s *Server) requestDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") {
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(apiReadTimeout))
			_ = controller.SetWriteDeadline(time.Now().Add(apiWriteTimeout))
		}
		next.ServeHTTP(w, r)
	})
}

// metricsHandler serves the Prometheus endpoint on the dedicated listener.
func (s *Server) metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.currentConfig()
		path := cfg.MetricsPath
		if path == "" {
			path = "/metrics"
		}
		if !cfg.MetricsEnabled || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		s.metrics.handler().ServeHTTP(w, r)
	})
}

// metricsOnMainListener reports whether /metrics is served on the public port.
func metricsOnMainListener(addr string) bool { return addr == "" }
