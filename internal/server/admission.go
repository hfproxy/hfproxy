package server

import (
	"context"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

// admitFill decides whether the miss is stored in the cache. When it is not,
// the file is streamed from the Hub to the client and the request is logged,
// so that a second request can be admitted; it reports false after answering.
// Preloads are always admitted: an administrator asked for the file.
func (s *Server) admitFill(res *fileResolution) bool {
	r := res.r
	if isPreloadFetch(r.Context()) || res.meta.Size > s.currentConfig().CacheMaxFileSize {
		return true // a file over the size limit is refused by the fill
	}
	admitted, reason := s.admissionDecision(r.Context(), res)
	if s.metrics != nil {
		result := "admitted"
		if !admitted {
			result = "refused"
		}
		s.metrics.add("hfproxy_cache_admissions_total", []string{"result=" + result}, 1)
	}
	if admitted {
		return true
	}
	s.logger.Debug("file streamed without being cached", "request_id", requestID(r.Context()), "repo_id", res.parsed.RepoID, "path", safeLogValue(res.parsed.FilePath, 300), "reason", reason)
	s.recordDownload(r.Context(), res.repoPK, res.commit, res.parsed.FilePath, "miss", res.meta.Size, "not_admitted")
	s.streamUncachedMiss(res.w, r, res.parsed, res.meta, res.commit, res.expectedLFS)
	return false
}

// admissionDecision applies the admission policy to the file of res. A
// failure to read the database admits the file, as before the policies existed.
func (s *Server) admissionDecision(ctx context.Context, res *fileResolution) (bool, string) {
	cfg := s.currentConfig()
	limits := cfg.CacheEviction.Normalized()
	admission := limits.Admission
	if admission.Policy == config.AdmitAlways {
		return true, "always"
	}
	if res.sizeKnown && res.meta.Size < admission.MinSize {
		return true, "small"
	}
	requests, err := s.db.RecentRequests(ctx, res.repoPK, res.commit, res.parsed.FilePath, admission.Window())
	if err != nil {
		s.logger.Warn("admission could not read the download log", "request_id", requestID(ctx))
		return true, "error"
	}
	switch admission.Policy {
	case config.AdmitSecondRequest:
		if requests > 0 {
			return true, "requested_before"
		}
		return false, "first_request"
	case config.AdmitFrequency:
		limit := cfg.CacheMaxTotalSize
		if limit <= 0 {
			return true, "no_limit"
		}
		used, err := s.db.LiveBytes(ctx)
		if err != nil {
			s.logger.Warn("admission could not read the cache size", "request_id", requestID(ctx))
			return true, "error"
		}
		if used+res.meta.Size <= limit {
			return true, "room"
		}
		policy := db.EvictionPolicy{Strategy: limits.Strategy, HalfLife: limits.HalfLife(), CostWeight: limits.Cost.Weight, CostBandwidth: limits.Cost.Bandwidth, CostLatency: limits.Cost.LatencySeconds}
		victims, err := s.db.EvictionCandidates(ctx, policy, db.EvictionScope{}, 1)
		if err != nil {
			s.logger.Warn("admission could not find the next eviction", "request_id", requestID(ctx))
			return true, "error"
		}
		if len(victims) == 0 {
			return true, "nothing_to_evict"
		}
		// This request counts too; a tie goes to what is already cached.
		if float64(requests+1) > victims[0].Heat {
			return true, "hotter_than_victim"
		}
		return false, "colder_than_victim"
	}
	return true, "always"
}
