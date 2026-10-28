package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
)

func TestIssue87NegativeCacheHasBoundedEntries(t *testing.T) {
	s := &Server{negative: make(map[string]negativeCacheEntry)}
	s.runtime.Store(&runtimeState{config: config.Config{CacheNegativeTTL: time.Minute}})
	now := time.Now()
	for i := 0; i < negativeCacheMaxEntries; i++ {
		s.negative[string(rune(i+1))] = negativeCacheEntry{expires: now.Add(time.Hour)}
	}
	s.negativeStore("new-entry", http.StatusNotFound, nil)
	if len(s.negative) != negativeCacheMaxEntries {
		t.Fatalf("negative cache entries = %d, want maximum %d", len(s.negative), negativeCacheMaxEntries)
	}
	if _, ok := s.negative["new-entry"]; !ok {
		t.Fatal("new negative entry was not retained")
	}
}
