package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

// settingsAdmin returns an administrator token principal backed by a real user,
// so audit rows can reference it.
func settingsAdmin(t *testing.T, store *db.Store) *Principal {
	t.Helper()
	id := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'admin')`, id, "settings-"+id[:8]); err != nil {
		t.Fatal(err)
	}
	return &Principal{ID: id, Username: "settings", Role: "admin", Authenticated: true, TokenID: "settings-token", Scopes: []string{"admin"}}
}

// settingsPayload does what the console does: read the settings and send them
// back, without the read-only *_set flags.
func settingsPayload(t *testing.T, s *Server, admin *Principal) map[string]any {
	t.Helper()
	rec := auditAPIDoAs(s, admin, http.MethodGet, "/api/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for key := range view {
		if strings.HasSuffix(key, "_set") {
			delete(view, key)
		}
	}
	return view
}

func putSettings(t *testing.T, s *Server, admin *Principal, payload map[string]any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := auditAPIDoAs(s, admin, http.MethodPut, "/api/v1/settings", string(body))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code == http.StatusOK {
		// The console keeps the revision of what it just saved.
		payload["revision"] = out["revision"]
	}
	return rec.Code, out
}

func TestTotalCacheLimitCanBeChangedFromTheConsole(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)

	view := settingsPayload(t, s, admin)
	if limit, ok := view["cache_max_total_size"]; !ok || limit.(float64) != 0 {
		t.Fatalf("the settings view must report the limit (0 = unlimited), got %v", view["cache_max_total_size"])
	}

	view["cache_max_total_size"] = float64(5 << 30)
	if status, out := putSettings(t, s, admin, view); status != http.StatusOK || out["cache_max_total_size"].(float64) != float64(5<<30) {
		t.Fatalf("PUT = %d %v", status, out)
	}
	if got := s.currentConfig().CacheMaxTotalSize; got != 5<<30 {
		t.Fatalf("the running server still uses %d, want the new limit applied at once", got)
	}
	saved, err := config.LoadPersisted(context.Background(), store.SQL, auditAPIConfig())
	if err != nil || saved.CacheMaxTotalSize != 5<<30 {
		t.Fatalf("saved limit = %d (%v), want it persisted across restarts", saved.CacheMaxTotalSize, err)
	}

	// A client that does not know the field must not remove the limit.
	older := settingsPayload(t, s, admin)
	delete(older, "cache_max_total_size")
	if status, _ := putSettings(t, s, admin, older); status != http.StatusOK {
		t.Fatalf("PUT without the field = %d", status)
	}
	if got := s.currentConfig().CacheMaxTotalSize; got != 5<<30 {
		t.Fatalf("leaving the field out changed the limit to %d", got)
	}

	negative := settingsPayload(t, s, admin)
	negative["cache_max_total_size"] = float64(-1)
	if status, _ := putSettings(t, s, admin, negative); status != http.StatusBadRequest {
		t.Fatalf("a negative limit = %d, want 400", status)
	}

	off := settingsPayload(t, s, admin)
	off["cache_max_total_size"] = float64(0)
	if status, _ := putSettings(t, s, admin, off); status != http.StatusOK || s.currentConfig().CacheMaxTotalSize != 0 {
		t.Fatalf("sending 0 must remove the limit: status %d, limit %d", status, s.currentConfig().CacheMaxTotalSize)
	}
}

func TestLoweringTheLimitEvictsWithoutWaitingForTheNextTick(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.LogLevel, cfg.DatabaseURL = "info", "postgres://unused" // applyConfig validates the whole configuration
	s := auditAPINewServer(t, store, cfg)
	s.maintenanceWake = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.startMaintenance(ctx) // the periodic tick is a minute away at first, then ten

	repo := seedRepository(t, store, "org/quota-live")
	seedPinnedArtifact(t, store, repo, pinCommitA, "old", 10, "2 days")
	seedPinnedArtifact(t, store, repo, pinCommitA, "new", 10, "1 day")

	cfg.CacheMaxTotalSize = 15
	if err := s.applyConfig(ctx, cfg, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if remainingArtifacts(t, store) == "new" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the new limit was not enforced within 5 seconds (artifacts left: %q); eviction waited for the periodic timer", remainingArtifacts(t, store))
}

func TestLockedSettingsCannotBeChangedFromTheConsole(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.LockedSettings = []string{"cache_max_total_size"}
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)

	view := settingsPayload(t, s, admin)
	locked, _ := view["locked"].([]any)
	if len(locked) != 1 {
		t.Fatalf("the view must list the locked settings, got %v", view["locked"])
	}

	// Sending the locked values back unchanged, as the console does, still works.
	view["cache_ref_ttl"] = "2m"
	if status, out := putSettings(t, s, admin, view); status != http.StatusOK {
		t.Fatalf("PUT of an unlocked change = %d %v", status, out)
	}

	changing := settingsPayload(t, s, admin)
	changing["cache_max_total_size"] = float64(7 << 30)
	status, out := putSettings(t, s, admin, changing)
	if status != http.StatusConflict {
		t.Fatalf("PUT changing a locked setting = %d, want 409", status)
	}
	if detail, _ := out["detail"].(string); !strings.Contains(detail, "cache_max_total_size") {
		t.Fatalf("the error must name the locked setting, got %q", detail)
	}
	if got := s.currentConfig().CacheMaxTotalSize; got != 0 {
		t.Fatalf("a refused change was applied: %d", got)
	}
}

// Two administrators editing at once, or one on a replica that missed the
// other's save, must not silently overwrite each other.
func TestSettingsSavedFromOutdatedSettingsAreRefused(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)

	one := settingsPayload(t, s, admin) // both read the same settings
	two := settingsPayload(t, s, admin)
	one["cache_max_total_size"] = 111
	if code, out := putSettings(t, s, admin, one); code != http.StatusOK {
		t.Fatalf("first save = %d %v", code, out)
	}
	two["download_rate_limit"] = 222
	if code, _ := putSettings(t, s, admin, two); code != http.StatusConflict {
		t.Fatalf("a save made on outdated settings = %d, want 409", code)
	}
	current := settingsPayload(t, s, admin)
	if current["cache_max_total_size"] != float64(111) || current["download_rate_limit"] != float64(0) {
		t.Fatalf("the refused save must change nothing, and the first must stay: %v %v", current["cache_max_total_size"], current["download_rate_limit"])
	}
	current["download_rate_limit"] = 222
	if code, out := putSettings(t, s, admin, current); code != http.StatusOK {
		t.Fatalf("a save made on the current settings = %d %v", code, out)
	}

	// A replica that missed those saves adopts them when asked, instead of
	// serving, and saving over, a stale copy.
	replica := auditAPINewServer(t, store, auditAPIConfig())
	view := settingsPayload(t, replica, admin)
	if view["cache_max_total_size"] != float64(111) || view["download_rate_limit"] != float64(222) {
		t.Fatalf("the replica shows stale settings: %v %v", view["cache_max_total_size"], view["download_rate_limit"])
	}
	if code, out := putSettings(t, replica, admin, view); code != http.StatusOK {
		t.Fatalf("a save from the refreshed replica = %d %v", code, out)
	}
}

// The console reads the eviction settings with their defaults filled in and
// sends them back. While they are locked to the environment, that round trip
// must not count as a change, or no other setting could be saved.
func TestLockedEvictionSettingsDoNotBlockOtherSaves(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.LockedSettings = []string{"cache_eviction"}
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)

	view := settingsPayload(t, s, admin)
	view["cache_ref_ttl"] = "2m"
	if status, out := putSettings(t, s, admin, view); status != http.StatusOK {
		t.Fatalf("PUT of an unrelated change with the eviction settings locked = %d %v", status, out)
	}

	changing := settingsPayload(t, s, admin)
	changing["cache_eviction"].(map[string]any)["strategy"] = "gdsf"
	if status, _ := putSettings(t, s, admin, changing); status != http.StatusConflict {
		t.Fatalf("PUT changing the locked strategy = %d, want 409", status)
	}
}
