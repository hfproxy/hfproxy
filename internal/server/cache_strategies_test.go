package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

// seedScored adds a file with what the strategies read: its heat and how long
// its fill took (0 = not measured).
func seedScored(t *testing.T, store *db.Store, repo, name string, size int64, age string, heat, fillSeconds float64) {
	t.Helper()
	seedPinnedArtifact(t, store, repo, pinCommitA, name, size, age)
	if _, err := store.SQL.Exec(`UPDATE artifacts SET heat=$1,heat_at=last_accessed_at,cached_at=last_accessed_at,fill_seconds=NULLIF($2::double precision,0) WHERE repository_id=$3 AND path=$4`, heat, fillSeconds, repo, name); err != nil {
		t.Fatal(err)
	}
}

func TestEachStrategyEvictsItsOwnFirstChoice(t *testing.T) {
	tests := []struct {
		name     string
		eviction config.CacheEviction
		keeps    string
	}{
		{"an empty strategy is LRU", config.CacheEviction{}, "busy,costly,recent"},
		{"lru", config.CacheEviction{Strategy: config.EvictLRU}, "busy,costly,recent"},
		// The same as LRU here: TestFIFOIgnoresAccessesAndHoldsItsRing tells them apart.
		{"fifo", config.CacheEviction{Strategy: config.EvictFIFO}, "busy,costly,recent"},
		// Used once, however recent or costly: the oldest of those goes only after
		// the decayed counts of the others are compared.
		{"lfu", config.CacheEviction{Strategy: config.EvictLFU}, "busy,costly,recent"},
		// Idle the longest, but an hour of fetching outweighs that.
		{"cost_lru", config.CacheEviction{Strategy: config.EvictCostLRU, Cost: config.CacheCost{Weight: 1}}, "busy,costly,recent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := auditAPIFreshStore(t)
			cfg := auditAPIConfig()
			cfg.CacheMaxTotalSize, cfg.CacheEviction = 3500, tc.eviction
			cfg.CacheEviction.TargetPercent = 99
			s := auditAPINewServer(t, store, cfg)
			repo := seedRepository(t, store, "org/strategies")
			seedScored(t, store, repo, "oldest", 1000, "4 days", 1, 1)
			seedScored(t, store, repo, "costly", 1000, "3 days", 1, 3600)
			seedScored(t, store, repo, "busy", 1000, "2 days", 50, 1)
			seedScored(t, store, repo, "recent", 1000, "1 day", 1, 1)
			if _, err := s.evictOverQuota(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := remainingArtifacts(t, store); got != tc.keeps {
				t.Fatalf("remaining = %q, want %q", got, tc.keeps)
			}
		})
	}
}

func TestStrategiesDisagreeWhereTheyShould(t *testing.T) {
	seed := func(t *testing.T, store *db.Store, repo string) {
		seedScored(t, store, repo, "cold-recent", 1000, "1 hour", 1, 10)
		seedScored(t, store, repo, "hot-old", 1000, "1 day", 30, 10)
		seedScored(t, store, repo, "huge-costly", 100000, "3 days", 2, 500)
	}
	for _, tc := range []struct {
		name     string
		eviction config.CacheEviction
		evicted  string
	}{
		{"lru drops what is idle the longest", config.CacheEviction{Strategy: config.EvictLRU}, "huge-costly"},
		{"lfu drops what is used the least", config.CacheEviction{Strategy: config.EvictLFU}, "cold-recent"},
		{"cost_lru spares what is slow to fetch", config.CacheEviction{Strategy: config.EvictCostLRU, Cost: config.CacheCost{Weight: 2}}, "hot-old"},
		{"gdsf weighs value per byte", config.CacheEviction{Strategy: config.EvictGDSF}, "huge-costly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := auditAPIFreshStore(t)
			repo := seedRepository(t, store, "org/disagree")
			seed(t, store, repo)
			limits := tc.eviction.Normalized()
			first, err := store.EvictionCandidates(context.Background(), db.EvictionPolicy{Strategy: limits.Strategy, HalfLife: limits.HalfLife(), CostWeight: limits.Cost.Weight, CostBandwidth: limits.Cost.Bandwidth, CostLatency: limits.Cost.LatencySeconds}, db.EvictionScope{}, 1)
			if err != nil || len(first) != 1 {
				t.Fatalf("candidates = %v (%v)", first, err)
			}
			var path string
			if err := store.SQL.QueryRow(`SELECT path FROM artifacts WHERE id=$1`, first[0].ArtifactID).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if path != tc.evicted {
				t.Fatalf("first candidate = %q, want %q", path, tc.evicted)
			}
		})
	}
}

func TestCostWeightDecidesHowLongACostlyFileMayIdle(t *testing.T) {
	for _, tc := range []struct {
		weight float64
		keeps  string
	}{{0.1, "cheap-recent"}, {1, "costly-old"}} {
		store := auditAPIFreshStore(t)
		cfg := auditAPIConfig()
		cfg.CacheMaxTotalSize = 15
		cfg.CacheEviction = config.CacheEviction{Strategy: config.EvictCostLRU, Cost: config.CacheCost{Weight: tc.weight}}
		s := auditAPINewServer(t, store, cfg)
		repo := seedRepository(t, store, "org/weights")
		seedScored(t, store, repo, "costly-old", 10, "10 days", 1, 100)
		seedScored(t, store, repo, "cheap-recent", 10, "1 day", 1, 1)
		if _, err := s.evictOverQuota(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := remainingArtifacts(t, store); got != tc.keeps {
			t.Fatalf("weight %v: remaining = %q, want %q", tc.weight, got, tc.keeps)
		}
	}
}

func TestAnUnmeasuredFileIsPricedFromItsSize(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 1<<30 + 1<<25
	cfg.CacheEviction = config.CacheEviction{Strategy: config.EvictCostLRU, TargetPercent: 99, Cost: config.CacheCost{Weight: 1, Bandwidth: 1 << 20, LatencySeconds: 1}}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/estimate")
	// 1024 seconds at 1 MiB/s, against a minute for the small one: idle for five
	// times as long, the big file still stays.
	seedScored(t, store, repo, "big-older", 1<<30, "5 days", 1, 0)
	seedScored(t, store, repo, "small", 1<<26, "1 day", 1, 0)
	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remainingArtifacts(t, store); got != "big-older" {
		t.Fatalf("remaining = %q, want the file that takes long to fetch", got)
	}
}

func TestFIFOIgnoresAccessesAndHoldsItsRing(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheEviction = config.CacheEviction{Strategy: config.EvictFIFO, FIFO: config.CacheFIFO{RingFiles: 2}}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/ring")
	seedScored(t, store, repo, "first", 10, "3 days", 1, 1)
	seedScored(t, store, repo, "second", 10, "2 days", 1, 1)
	seedScored(t, store, repo, "third", 10, "1 day", 1, 1)
	if _, err := store.SQL.Exec(`UPDATE artifacts SET last_accessed_at=now() WHERE path='first'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.evictOverQuota(context.Background()); err != nil || n != 1 {
		t.Fatalf("evicted %d (%v), want 1", n, err)
	}
	if got := remainingArtifacts(t, store); got != "second,third" {
		t.Fatalf("remaining = %q, want the two newest entries", got)
	}
}

func TestTheRingInBytesAppliesWithoutATotalLimit(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheEviction = config.CacheEviction{Strategy: config.EvictFIFO, FIFO: config.CacheFIFO{RingSize: 15}, TargetPercent: 99}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/ring-bytes")
	seedScored(t, store, repo, "first", 10, "2 days", 1, 1)
	seedScored(t, store, repo, "second", 10, "1 day", 1, 1)
	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remainingArtifacts(t, store); got != "second" {
		t.Fatalf("remaining = %q", got)
	}
}

func TestTheRingBelongsToTheFIFOStrategy(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheEviction = config.CacheEviction{Strategy: config.EvictLRU, FIFO: config.CacheFIFO{RingSize: 1, RingFiles: 1}}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/not-a-ring")
	seedScored(t, store, repo, "first", 10, "2 days", 1, 1)
	seedScored(t, store, repo, "second", 10, "1 day", 1, 1)
	if n, err := s.evictOverQuota(context.Background()); err != nil || n != 0 {
		t.Fatalf("LRU evicted %d (%v) because of the settings of another strategy", n, err)
	}
}

func TestRepositoryAndNamespaceLimits(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheEviction = config.CacheEviction{MaxRepositorySize: 25, MaxNamespaceSize: 35, TargetPercent: 99}
	s := auditAPINewServer(t, store, cfg)
	big := seedRepository(t, store, "big-org/huge")
	seedPinnedArtifact(t, store, big, pinCommitA, "huge-old", 10, "5 days")
	seedPinnedArtifact(t, store, big, pinCommitA, "huge-mid", 10, "4 days")
	seedPinnedArtifact(t, store, big, pinCommitA, "huge-new", 10, "1 hour")
	sibling := seedRepository(t, store, "big-org/sibling")
	seedPinnedArtifact(t, store, sibling, pinCommitA, "sibling-old", 10, "6 days")
	seedPinnedArtifact(t, store, sibling, pinCommitA, "sibling-new", 10, "2 hours")
	other := seedRepository(t, store, "other/small")
	seedPinnedArtifact(t, store, other, pinCommitA, "other-oldest", 20, "30 days")

	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	// huge drops to 20 bytes for its own limit; the namespace, then at 40, drops
	// the oldest file it holds. Another namespace is left alone, however old.
	if got := remainingArtifacts(t, store); got != "huge-mid,huge-new,other-oldest,sibling-new" {
		t.Fatalf("remaining = %q", got)
	}
}

func TestARepositoryLimitCountsASharedBlobOnce(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheEviction = config.CacheEviction{MaxRepositorySize: 15, TargetPercent: 99}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/shared")
	blob := seedPinnedArtifact(t, store, repo, pinCommitA, "weights", 10, "3 days")
	if _, err := store.SQL.Exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id) VALUES($1,$2,$3,'weights',$4)`, db.NewID(), repo, pinCommitB, blob); err != nil {
		t.Fatal(err)
	}
	if n, err := s.evictOverQuota(context.Background()); err != nil || n != 0 {
		t.Fatalf("evicted %d (%v), but the repository holds 10 distinct bytes", n, err)
	}
}

func TestIdleAndRevisionLimitsSparePinsAndPublishedContent(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheEviction = config.CacheEviction{MaxIdleHours: 24 * 7, MaxRevisions: 1}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/limits")
	seedPinnedArtifact(t, store, repo, pinCommitA, "current", 10, "1 hour")
	seedPinnedArtifact(t, store, repo, pinCommitB, "older-commit", 10, "2 hours")
	pinned := seedRepository(t, store, "org/pinned")
	seedPinnedArtifact(t, store, pinned, pinCommitA, "pinned-idle", 10, "60 days")
	seedPinnedArtifact(t, store, pinned, pinCommitB, "pinned-old-commit", 10, "90 days")
	if _, err := store.SQL.Exec(`INSERT INTO cache_pins(repository_id,commit_sha) VALUES($1,'')`, pinned); err != nil {
		t.Fatal(err)
	}
	idle := seedRepository(t, store, "org/idle")
	seedPinnedArtifact(t, store, idle, pinCommitA, "forgotten", 10, "8 days")
	local := seedRepository(t, store, "org/local")
	seedPinnedArtifact(t, store, local, pinCommitA, "published", 10, "90 days")
	if _, err := store.SQL.Exec(`UPDATE repositories SET origin='local' WHERE id=$1`, local); err != nil {
		t.Fatal(err)
	}
	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remainingArtifacts(t, store); got != "current,pinned-idle,pinned-old-commit,published" {
		t.Fatalf("remaining = %q", got)
	}
}

func TestAHeadDoesNotHideTheDownloadThatFollowsIt(t *testing.T) {
	store := auditAPIFreshStore(t)
	repo := seedRepository(t, store, "org/heat")
	seedScored(t, store, repo, "file", 10, "1 hour", 4, 1)
	ctx := context.Background()
	var id string
	if err := store.SQL.QueryRow(`SELECT id FROM artifacts`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	heat := func() (value float64) {
		t.Helper()
		if err := store.SQL.QueryRow(`SELECT heat FROM artifacts`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	halfLife := time.Hour
	if err := store.TouchArtifact(ctx, id, false, halfLife); err != nil {
		t.Fatal(err)
	}
	if got := heat(); got != 4 {
		t.Fatalf("a HEAD changed the heat to %v", got)
	}
	if err := store.TouchArtifact(ctx, id, true, halfLife); err != nil {
		t.Fatal(err)
	}
	// Four uses an hour old count for two, plus this one.
	if got := heat(); got < 2.99 || got > 3.01 {
		t.Fatalf("heat = %v, want about 3", got)
	}
	if err := store.TouchArtifact(ctx, id, true, halfLife); err != nil {
		t.Fatal(err)
	}
	if got := heat(); got > 3.01 {
		t.Fatalf("a second download within the minute counted: %v", got)
	}
	// A file idle for centuries must not make the arithmetic fail.
	if _, err := store.SQL.Exec(`UPDATE artifacts SET heat_at=now()-interval '500 years'`); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchArtifact(ctx, id, true, halfLife); err != nil {
		t.Fatal(err)
	}
	if got := heat(); got < 0.99 || got > 1.01 {
		t.Fatalf("heat = %v, want about 1", got)
	}
}

func TestEvictionSettingsCanBeChangedFromTheConsole(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)

	view := settingsPayload(t, s, admin)
	eviction := view["cache_eviction"].(map[string]any)
	if eviction["strategy"] != "lru" || eviction["target_percent"].(float64) != 90 {
		t.Fatalf("the view must report the effective defaults, got %v", eviction)
	}
	eviction["strategy"] = "gdsf"
	eviction["max_repository_size"] = float64(1 << 30)
	eviction["fifo"].(map[string]any)["ring_files"] = float64(1000)
	if status, out := putSettings(t, s, admin, view); status != http.StatusOK {
		t.Fatalf("PUT = %d %v", status, out)
	}
	got := s.currentConfig().CacheEviction
	if got.Strategy != config.EvictGDSF || got.MaxRepositorySize != 1<<30 || got.FIFO.RingFiles != 1000 {
		t.Fatalf("running settings = %+v", got)
	}
	saved, err := config.LoadPersisted(context.Background(), store.SQL, auditAPIConfig())
	if err != nil || saved.CacheEviction != got {
		t.Fatalf("saved settings = %+v (%v), want %+v", saved.CacheEviction, err, got)
	}

	older := settingsPayload(t, s, admin)
	delete(older, "cache_eviction")
	if status, _ := putSettings(t, s, admin, older); status != http.StatusOK || s.currentConfig().CacheEviction != got {
		t.Fatalf("leaving the field out changed the settings: %d %+v", status, s.currentConfig().CacheEviction)
	}

	unknown := settingsPayload(t, s, admin)
	unknown["cache_eviction"].(map[string]any)["strategy"] = "random"
	if status, _ := putSettings(t, s, admin, unknown); status != http.StatusBadRequest {
		t.Fatalf("an unknown strategy = %d, want 400", status)
	}
}
