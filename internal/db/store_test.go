package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	sqlDB := isolatedDatabase(t)
	if err := Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	return &Store{SQL: sqlDB}
}

func TestUsersAndTheLastAdministrator(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	admin, reader := NewID(), NewID()
	if err := store.CreateUser(ctx, admin, "root", "", "", "hash", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(ctx, reader, "alice", "a@example.com", "Alice", "hash", "reader"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(ctx, NewID(), "alice", "", "", "hash", "reader"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate username = %v, want ErrExists", err)
	}
	users, err := store.ListUsers(ctx, nil, 10)
	if err != nil || len(users) != 2 || users[0].Username != "alice" {
		t.Fatalf("ListUsers = %v %v", users, err)
	}
	if next, _ := store.ListUsers(ctx, &[2]string{"alice", reader}, 10); len(next) != 1 || next[0].Username != "root" {
		t.Fatalf("keyset page = %v", next)
	}
	if err := store.UpdateUser(ctx, admin, "reader", ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the only admin = %v, want ErrLastAdmin", err)
	}
	if err := store.UpdateUser(ctx, admin, "", "disabled"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the only admin = %v, want ErrLastAdmin", err)
	}
	if err := store.UpdateUser(ctx, NewID(), "reader", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	// Disabling revokes tokens and ends sessions in the same step.
	hash := []byte("0123456789abcdef0123456789abcdef")
	tok := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read"}}
	if err := store.CreateToken(ctx, reader, tok, hash, "", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL.Exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$1,now()+interval '1 hour')`, []byte("session-hash-session-hash-session"), reader); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateUser(ctx, reader, "", "disabled"); err != nil {
		t.Fatal(err)
	}
	var live, sessions int
	store.SQL.QueryRow(`SELECT count(*) FROM access_tokens WHERE user_id=$1 AND revoked_at IS NULL`, reader).Scan(&live)
	store.SQL.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=$1`, reader).Scan(&sessions)
	if live != 0 || sessions != 0 {
		t.Fatalf("after disabling: %d live tokens, %d sessions; want none", live, sessions)
	}
}

func TestTokenLimitAndInactiveUsers(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	store.CreateUser(ctx, user, "bob", "", "", "hash", "reader")
	for i := 0; i < 2; i++ {
		tok := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read"}}
		if err := store.CreateToken(ctx, user, tok, []byte{byte(i), 1, 2, 3}, "", 2); err != nil {
			t.Fatal(err)
		}
		if tok.CreatedAt.IsZero() {
			t.Fatal("CreatedAt was not filled")
		}
	}
	over := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read"}}
	if err := store.CreateToken(ctx, user, over, []byte{9, 9, 9}, "", 2); !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("third token = %v, want ErrTokenLimit", err)
	}
	tokens, _ := store.ListTokens(ctx, user)
	if len(tokens) != 2 {
		t.Fatalf("ListTokens = %d", len(tokens))
	}
	if ok, err := store.RevokeToken(ctx, user, tokens[0].ID); !ok || err != nil {
		t.Fatalf("RevokeToken = %v %v", ok, err)
	}
	if ok, _ := store.RevokeToken(ctx, user, tokens[0].ID); ok {
		t.Fatal("a token can be revoked once")
	}
	if err := store.CreateToken(ctx, NewID(), &AccessToken{ID: NewID(), Scopes: []string{"read"}}, []byte{7}, "", 2); !errors.Is(err, ErrInactiveUser) {
		t.Fatalf("unknown user = %v, want ErrInactiveUser", err)
	}
}

func TestGroupsRepositoriesAndPins(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	store.CreateUser(ctx, user, "carol", "", "", "hash", "reader")
	group := NewID()
	if err := store.CreateGroup(ctx, group, "ml", []string{user, user}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGroup(ctx, NewID(), "ml", nil); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate group = %v", err)
	}
	if err := store.CreateGroup(ctx, NewID(), "ghosts", []string{NewID()}); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("unknown member = %v", err)
	}
	if g, members, err := store.GetGroup(ctx, group); err != nil || g.Name != "ml" || len(members) != 1 {
		t.Fatalf("GetGroup = %v %v %v", g, members, err)
	}
	renamed := "ml-team"
	if name, count, err := store.UpdateGroup(ctx, group, &renamed, &[]string{}); err != nil || name != renamed || count != 0 {
		t.Fatalf("UpdateGroup = %q %d %v", name, count, err)
	}
	if _, _, err := store.UpdateGroup(ctx, NewID(), &renamed, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group = %v", err)
	}

	repo, err := store.UpsertRepository(ctx, "models", "org/private", "allowlist", user)
	if err != nil {
		t.Fatal(err)
	}
	visible := func(role, who string) int {
		list, err := store.ListRepositories(ctx, role, who, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	if visible("reader", user) != 0 || visible("admin", "x") != 1 {
		t.Fatal("an allowlisted repository must be hidden from users without a grant and visible to admins")
	}
	if err := store.UpsertGrant(ctx, repo, "user", user, "read"); err != nil {
		t.Fatal(err)
	}
	if visible("reader", user) != 1 {
		t.Fatal("a granted user must see the repository")
	}
	if ok, _ := store.GrantSubjectExists(ctx, "group", group); !ok {
		t.Fatal("the group should exist")
	}
	if added, _ := store.AddPin(ctx, repo, "", "main", user); !added {
		t.Fatal("first pin should be new")
	}
	if added, _ := store.AddPin(ctx, repo, "", "main", user); added {
		t.Fatal("pinning twice is a no-op")
	}
	if pins, _ := store.ListPins(ctx, repo); len(pins) != 1 || pins[0].Ref != "main" {
		t.Fatalf("ListPins = %v", pins)
	}
	if removed, _ := store.RemovePin(ctx, repo, "", "main"); !removed {
		t.Fatal("pin should have been removed")
	}
	if err := store.DeleteRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRepository(ctx, repo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted repository = %v", err)
	}
	if grants, _ := store.ListGrants(ctx, repo); len(grants) != 0 {
		t.Fatal("deleting a repository removes its grants")
	}
}

func TestPublishArtifactRestoresMissingObjectsAndRecordsDownloads(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "owner", "", "", "hash", "admin")
	repo, err := store.UpsertRepository(ctx, "models", "org/m", "public", owner)
	if err != nil {
		t.Fatal(err)
	}
	in := PublishInput{RepoID: repo, Commit: "0123456789abcdef0123456789abcdef01234567", Path: "w.bin", SHA256: "aa", Size: 3, StorageKey: "blobs/aa", ContentType: "application/octet-stream", ETag: "e", ObjectStored: true}
	called := false
	if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { called = true; return nil }); err != nil || called {
		t.Fatalf("first publish: err=%v, ensureObject called=%v (a new blob needs no check)", err, called)
	}
	in.Path = "other.bin"
	boom := errors.New("storage down")
	if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("a storage failure must come back unchanged, got %v", err)
	}
	if _, err := store.FindArtifact(ctx, repo, in.Commit, "other.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed publish must leave nothing behind")
	}
	if err := store.PublishArtifact(ctx, in, func(_ context.Context, key string) error {
		if key != "blobs/aa" {
			t.Errorf("ensureObject got key %q", key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := store.FindArtifact(ctx, repo, in.Commit, "other.bin")
	if err != nil || a.SHA256 != "aa" {
		t.Fatalf("FindArtifact = %v %v", a, err)
	}
	if err := store.RecordDownloads(ctx, []DownloadEvent{
		{RepoID: repo, Commit: in.Commit, Path: "w.bin", Status: "hit", Bytes: 3, Caller: Caller{ClientIP: "10.0.0.1"}},
		{RepoID: repo, Commit: in.Commit, Path: "w.bin", Status: "hit", Bytes: 3},
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.CacheStats(ctx, "admin", "x")
	if err != nil || stats.Hits != 2 || stats.Artifacts != 2 {
		t.Fatalf("CacheStats = %+v %v", stats, err)
	}
	entries, _ := store.SearchCache(ctx, "admin", "x", "", "OTHER", 10)
	if len(entries) != 1 || entries[0].Path != "other.bin" {
		t.Fatalf("SearchCache is case-insensitive on the path, got %v", entries)
	}
	if err := store.PurgeArtifact(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.PurgeArtifact(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second purge = %v", err)
	}
	if count, _, _ := store.PurgeEstimate(ctx, "admin", "x", "", ""); count != 1 {
		t.Fatalf("one artifact should remain, estimate says %d", count)
	}
}

func TestSessionsTokensAndAudit(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	store.CreateUser(ctx, user, "gina", "g@example.com", "Gina", "pw-hash", "reader")

	hash, csrf := []byte("session-hash-0123456789abcdef-aa"), []byte("csrf-hash-0123456789abcdef-aaaaa")
	if err := store.CreateSession(ctx, hash, user, csrf, time.Now().Add(time.Hour), "k1"); err != nil {
		t.Fatal(err)
	}
	found, err := store.LookupSession(ctx, [][]byte{[]byte("unknown"), hash})
	if err != nil || found.ID != user || found.Username != "gina" || !bytes.Equal(found.Hash, hash) {
		t.Fatalf("LookupSession = %+v %v", found, err)
	}
	if stored, err := store.SessionCSRFHash(ctx, [][]byte{hash}); err != nil || !bytes.Equal(stored, csrf) {
		t.Fatalf("SessionCSRFHash = %q %v", stored, err)
	}
	if owner := store.SessionOwner(ctx, [][]byte{hash}); owner != user {
		t.Fatalf("SessionOwner = %q", owner)
	}
	if err := store.TouchSession(ctx, hash, time.Hour, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSessions(ctx, [][]byte{hash}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LookupSession(ctx, [][]byte{hash}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted session = %v, want ErrNotFound", err)
	}
	expired := []byte("expired-session-hash-0123456789a")
	store.CreateSession(ctx, expired, user, csrf, time.Now().Add(-time.Minute), "")
	if _, err := store.LookupSession(ctx, [][]byte{expired}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session = %v, want ErrNotFound", err)
	}

	tok := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read", "write"}}
	tokenHash := []byte("token-hash-0123456789abcdef-old!")
	if err := store.CreateToken(ctx, user, tok, tokenHash, "k0", 10); err != nil {
		t.Fatal(err)
	}
	lookup, err := store.LookupToken(ctx, [][]byte{tokenHash})
	if err != nil || lookup.TokenID != tok.ID || len(lookup.Scopes) != 2 || !bytes.Equal(lookup.StoredHash, tokenHash) {
		t.Fatalf("LookupToken = %+v %v", lookup, err)
	}
	newHash := []byte("token-hash-0123456789abcdef-new!")
	if err := store.RehashToken(ctx, tok.ID, tokenHash, newHash, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LookupToken(ctx, [][]byte{tokenHash}); !errors.Is(err, ErrNotFound) {
		t.Fatal("the old hash must stop working after a rehash")
	}
	if err := store.TouchToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	store.RevokeToken(ctx, user, tok.ID)
	if _, err := store.LookupToken(ctx, [][]byte{newHash}); !errors.Is(err, ErrNotFound) {
		t.Fatal("a revoked token must not be found")
	}

	if id, hashStored, status, err := store.PasswordLogin(ctx, "gina"); err != nil || id != user || hashStored != "pw-hash" || status != "active" {
		t.Fatalf("PasswordLogin = %q %q %q %v", id, hashStored, status, err)
	}
	if _, _, _, err := store.PasswordLogin(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown login = %v", err)
	}
	if err := store.InsertAudit(ctx, user, "test.event", "thing", "1", `{"k":"v"}`, "req-1"); err != nil {
		t.Fatal(err)
	}
	events, _ := store.ListAudit(ctx, nil, 5)
	if len(events) != 1 || events[0].Action != "test.event" || events[0].Actor != "gina" {
		t.Fatalf("audit = %+v", events)
	}
}

func TestExternalIdentitiesAndGroupSync(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	if err := store.ProvisionExternalUser(ctx, user, "hank-abc", "h@example.com", "Hank", "oidc", "https://idp", "sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.ProvisionExternalUser(ctx, NewID(), "hank-abc", "", "", "oidc", "https://idp", "sub-2"); err == nil {
		t.Fatal("a duplicate username must fail")
	} else {
		var stage *StageError
		if !errors.As(err, &stage) || stage.Stage != "user" {
			t.Fatalf("error = %v, want the user stage", err)
		}
	}
	account, err := store.FindExternalIdentity(ctx, "https://idp", "sub-1")
	if err != nil || account.UserID != user || account.Status != "active" {
		t.Fatalf("FindExternalIdentity = %+v %v", account, err)
	}
	if _, err := store.FindExternalIdentity(ctx, "https://idp", "sub-404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown identity = %v", err)
	}
	if err := store.ChangeExternalSubject(ctx, account, "sub-1-renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindExternalIdentity(ctx, "https://idp", "sub-1-renamed"); err != nil {
		t.Fatal(err)
	}

	localGroup := NewID()
	store.SQL.Exec(`INSERT INTO groups(id,name,source) VALUES($1,'shared','local')`, localGroup)
	store.SQL.Exec(`INSERT INTO group_members(group_id,user_id) VALUES($1,$2)`, localGroup, user)
	if err := store.SyncGroups(ctx, user, "ldap", []string{"a", "b", "shared", ""}); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncGroups(ctx, user, "ldap", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.SQL.Query(`SELECT g.name,g.source FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=$1 ORDER BY 1`, user)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, source string
		rows.Scan(&name, &source)
		got = append(got, name+":"+source)
	}
	if strings.Join(got, ",") != "a:ldap,shared:local" {
		t.Fatalf("memberships = %v, want the ldap group a and the untouched local group shared", got)
	}
}

func TestLoginAttemptCounters(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	window := time.Minute
	for i := 1; i <= 3; i++ {
		counts, err := store.CountLoginAttempts(ctx, []string{"ip:1", "pair:1:x"}, window)
		if err != nil || len(counts) != 2 {
			t.Fatalf("CountLoginAttempts = %v %v", counts, err)
		}
		for _, c := range counts {
			if c.Attempts != i || c.Remaining < 1 || c.Remaining > 60 {
				t.Fatalf("attempt %d: %+v", i, c)
			}
		}
	}
	if _, found, err := store.LoginAttempts(ctx, "fail:x", window); err != nil || found {
		t.Fatalf("an unseen key = found %v, %v", found, err)
	}
	store.AddLoginAttempt(ctx, "fail:x", window)
	store.AddLoginAttempt(ctx, "fail:x", window)
	if c, found, err := store.LoginAttempts(ctx, "fail:x", window); err != nil || !found || c.Attempts != 2 {
		t.Fatalf("LoginAttempts = %+v %v %v", c, found, err)
	}
	// A key whose window ended starts over.
	store.SQL.Exec(`UPDATE login_attempts SET window_start=now()-interval '2 minutes' WHERE key='fail:x'`)
	store.AddLoginAttempt(ctx, "fail:x", window)
	if c, _, _ := store.LoginAttempts(ctx, "fail:x", window); c.Attempts != 1 {
		t.Fatalf("after the window expired the count is %d, want 1", c.Attempts)
	}
}

func TestEvictionRetentionAndBlobCollection(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "ivy", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/e", "public", owner)
	commit := "0123456789abcdef0123456789abcdef01234567"
	publish := func(path, sha string, size int64) {
		in := PublishInput{RepoID: repo, Commit: commit, Path: path, SHA256: sha, Size: size, StorageKey: "blobs/" + sha, ContentType: "x"}
		if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	publish("old", "aa", 10)
	publish("new", "bb", 20)
	store.SQL.Exec(`UPDATE artifacts SET last_accessed_at=now()-interval '2 days' WHERE path='old'`)
	if total, _ := store.LiveBytes(ctx); total != 30 {
		t.Fatalf("LiveBytes = %d", total)
	}
	if count, size, _ := store.BlobTotals(ctx); count != 2 || size != 30 {
		t.Fatalf("BlobTotals = %d %d", count, size)
	}
	cands, err := store.EvictionCandidates(ctx, EvictionPolicy{}, EvictionScope{}, 10)
	if err != nil || len(cands) != 2 {
		t.Fatalf("EvictionCandidates = %v %v", cands, err)
	}
	a, _ := store.FindArtifact(ctx, repo, commit, "old")
	if cands[0].ArtifactID != a.ID {
		t.Fatal("the least recently used artifact must come first")
	}
	// A pin keeps a file out of the candidates and counts as pinned bytes.
	store.AddPin(ctx, repo, commit, "", owner)
	if cands, _ := store.EvictionCandidates(ctx, EvictionPolicy{}, EvictionScope{}, 10); len(cands) != 0 {
		t.Fatalf("pinned files must not be candidates: %v", cands)
	}
	if pinned, _ := store.PinnedBytes(ctx); pinned != 30 {
		t.Fatalf("PinnedBytes = %d", pinned)
	}
	store.RemovePin(ctx, repo, commit, "")

	freed, err := store.EvictArtifact(ctx, a.ID, a.BlobID)
	if err != nil || freed != 10 {
		t.Fatalf("EvictArtifact freed %d (%v), want 10", freed, err)
	}
	// Orphaned blobs are collected only after the grace period, and the object is deleted first.
	if ids, _ := store.OrphanedBlobIDs(ctx, time.Hour); len(ids) != 0 {
		t.Fatalf("a fresh orphan must wait for the grace period: %v", ids)
	}
	store.SQL.Exec(`UPDATE blobs SET orphaned_at=now()-interval '2 hours' WHERE sha256='aa'`)
	ids, _ := store.OrphanedBlobIDs(ctx, time.Hour)
	if len(ids) != 1 {
		t.Fatalf("OrphanedBlobIDs = %v", ids)
	}
	deleted := ""
	removed, err := store.CollectOrphanedBlob(ctx, ids[0], time.Hour, func(_ context.Context, key string) error { deleted = key; return nil })
	if err != nil || !removed || deleted != "blobs/aa" {
		t.Fatalf("CollectOrphanedBlob = %v %v key=%q", removed, err, deleted)
	}
	// An object that cannot be deleted keeps its row, so nothing leaks.
	store.SQL.Exec(`UPDATE blobs SET orphaned_at=now()-interval '2 hours' WHERE sha256='bb'`)
	store.SQL.Exec(`DELETE FROM artifacts WHERE path='new'`)
	ids, _ = store.OrphanedBlobIDs(ctx, time.Hour)
	if removed, err := store.CollectOrphanedBlob(ctx, ids[0], time.Hour, func(context.Context, string) error { return errors.New("S3 down") }); removed || err == nil {
		t.Fatalf("a failed object delete must be reported: %v %v", removed, err)
	}
	if count, _, _ := store.BlobTotals(ctx); count != 1 {
		t.Fatalf("the blob row must survive a failed object delete, %d rows", count)
	}

	// Retention.
	store.SQL.Exec(`INSERT INTO downloads(id,path,status,started_at) VALUES('d-old','x','hit',now()-interval '10 days'),('d-new','y','hit',now())`)
	if n, err := store.RunRetention(ctx, RetainDownloads, 100, 48*time.Hour); err != nil || n != 1 {
		t.Fatalf("RunRetention(downloads) = %d %v", n, err)
	}
	if _, err := store.RunRetention(ctx, RetentionJob("nope"), 1, 0); err == nil {
		t.Fatal("an unknown job must be refused")
	}
}

func TestResumptionLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "jo", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/r", "public", owner)
	if _, err := store.FindResumption(ctx, repo, "c", "f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing state = %v", err)
	}
	r := &Resumption{ID: NewID(), RepositoryID: repo, CommitSHA: "c", Path: "f", ETag: "e1", TotalSize: 100, ContentType: "x", ChunkSize: 40}
	if err := store.CreateResumption(ctx, r); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AdvanceResumption(ctx, r.ID, 0, 40); err != nil || !ok {
		t.Fatalf("advance = %v %v", ok, err)
	}
	if ok, _ := store.AdvanceResumption(ctx, r.ID, 0, 40); ok {
		t.Fatal("a stale writer must not advance the count")
	}
	got, _ := store.FindResumption(ctx, repo, "c", "f")
	if got.CompletedBytes != 40 || got.ETag != "e1" {
		t.Fatalf("state = %+v", got)
	}
	store.SetResumptionETag(ctx, r.ID, "e2")
	store.ResetResumption(ctx, r.ID, "e3", 200, "y", 50)
	got, _ = store.FindResumption(ctx, repo, "c", "f")
	if got.CompletedBytes != 0 || got.ETag != "e3" || got.TotalSize != 200 || got.ChunkSize != 50 {
		t.Fatalf("after reset = %+v", got)
	}

	// Stale cleanup: only old rows, and the blocks are discarded first.
	if stale, _ := store.StaleResumptions(ctx, time.Hour, 10); len(stale) != 0 {
		t.Fatalf("fresh state listed as stale: %v", stale)
	}
	store.SQL.Exec(`UPDATE download_resumptions SET updated_at=now()-interval '3 hours' WHERE id=$1`, r.ID)
	stale, _ := store.StaleResumptions(ctx, time.Hour, 10)
	if len(stale) != 1 {
		t.Fatalf("stale = %v", stale)
	}
	failing := func(context.Context, *Resumption) error { return errors.New("S3 down") }
	if removed, err := store.CleanupStaleResumption(ctx, stale[0], time.Hour, failing); removed || err == nil {
		t.Fatalf("a failed discard must keep the state: %v %v", removed, err)
	}
	discarded := false
	ok := func(context.Context, *Resumption) error { discarded = true; return nil }
	if removed, err := store.CleanupStaleResumption(ctx, stale[0], time.Hour, ok); !removed || err != nil || !discarded {
		t.Fatalf("cleanup = %v %v discarded=%v", removed, err, discarded)
	}
	if _, err := store.FindResumption(ctx, repo, "c", "f"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the state must be gone")
	}

	// The advisory lock excludes a second holder until released.
	unlock, err := store.LockResume(ctx, repo, "c", "f")
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan struct{})
	go func() {
		u, err := store.LockResume(ctx, repo, "c", "f")
		if err == nil {
			u()
		}
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("the lock was granted twice")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("the lock was not released")
	}
}

func TestFirstAdminIsCreatedOnlyOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if has, err := store.HasActiveAdmin(ctx); err != nil || has {
		t.Fatalf("fresh database: HasActiveAdmin = %v, %v", has, err)
	}
	// A reader or a disabled administrator does not count.
	store.CreateUser(ctx, NewID(), "reader1", "", "", "h", "reader")
	disabled := NewID()
	store.CreateUser(ctx, disabled, "olduser", "", "", "h", "admin")
	store.SQL.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, disabled)
	if has, _ := store.HasActiveAdmin(ctx); has {
		t.Fatal("only an active administrator counts")
	}
	if err := store.CreateFirstAdmin(ctx, NewID(), "olduser", "", "", "h"); !errors.Is(err, ErrExists) {
		t.Fatalf("a taken username = %v, want ErrExists", err)
	}

	// Many simultaneous setups: exactly one wins.
	const attempts = 12
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			results <- store.CreateFirstAdmin(ctx, NewID(), "boss"+strings.Repeat("x", i), "", "", "h")
		}(i)
	}
	created, refused := 0, 0
	for i := 0; i < attempts; i++ {
		switch err := <-results; {
		case err == nil:
			created++
		case errors.Is(err, ErrSetupDone):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if created != 1 || refused != attempts-1 {
		t.Fatalf("created %d administrators, refused %d; want exactly one created", created, refused)
	}
	var admins int
	store.SQL.QueryRow(`SELECT count(*) FROM users WHERE role='admin' AND status='active'`).Scan(&admins)
	if admins != 1 {
		t.Fatalf("%d active administrators", admins)
	}
	if has, _ := store.HasActiveAdmin(ctx); !has {
		t.Fatal("HasActiveAdmin must see the new administrator")
	}
}

func TestUserQuotaAndDownloadUsage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	store.CreateUser(ctx, user, "quota-user", "", "", "h", "reader")
	repo, _ := store.UpsertRepository(ctx, "models", "org/quota", "public", user)

	if any, _ := store.AnyUserDailyQuota(ctx); any {
		t.Fatal("no user has a quota yet")
	}
	quota, used, err := store.DownloadUsage(ctx, user)
	if err != nil || quota != nil || used != 0 {
		t.Fatalf("a new user: quota=%v used=%d err=%v", quota, used, err)
	}
	// Only the last 24 hours of this user's downloads count.
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO downloads(id,repository_id,path,status,bytes,user_id,started_at) VALUES('q1',$1,'a','hit',100,$2,now()),('q2',$1,'b','hit',50,$2,now()-interval '2 hours'),('q3',$1,'c','hit',999,$2,now()-interval '3 days'),('q4',$1,'d','hit',7,'someone-else',now())`, repo, user)
	if _, used, _ := store.DownloadUsage(ctx, user); used != 150 {
		t.Fatalf("used = %d, want 150 (the 3-day-old and the other user's downloads do not count)", used)
	}
	ten := int64(10)
	if err := store.SetUserDailyQuota(ctx, user, &ten); err != nil {
		t.Fatal(err)
	}
	if quota, _, _ := store.DownloadUsage(ctx, user); quota == nil || *quota != 10 {
		t.Fatalf("quota = %v, want 10", quota)
	}
	if any, _ := store.AnyUserDailyQuota(ctx); !any {
		t.Fatal("a user has a quota")
	}
	zero := int64(0)
	store.SetUserDailyQuota(ctx, user, &zero)
	if any, _ := store.AnyUserDailyQuota(ctx); any {
		t.Fatal("0 means unlimited and is not a limit to enforce")
	}
	if err := store.SetUserDailyQuota(ctx, user, nil); err != nil {
		t.Fatal(err)
	}
	if quota, _, _ := store.DownloadUsage(ctx, user); quota != nil {
		t.Fatalf("a cleared quota = %v", quota)
	}
	if err := store.SetUserDailyQuota(ctx, "nobody", &ten); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown user = %v, want ErrNotFound", err)
	}
	if q, u, err := store.DownloadUsage(ctx, "nobody"); err != nil || q != nil || u != 0 {
		t.Fatalf("an unknown user's usage = %v %d %v", q, u, err)
	}
}

// A fill that saw a blob row and therefore skipped the upload can publish after the
// collector deleted that very blob: its lookup waits for the collector's row lock,
// finds no row once the collector commits, and records a new one. The object the
// collector removed must be put back, or the catalog points at nothing forever.
func TestPublishAfterTheCollectorRemovedTheBlobRestoresTheObject(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "gc", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/gc", "public", owner)
	commit := "0123456789abcdef0123456789abcdef01234567"
	const key = "blobs/gc"
	objects := map[string]bool{key: true}
	var objectsMu sync.Mutex
	stored := func() bool { objectsMu.Lock(); defer objectsMu.Unlock(); return objects[key] }

	in := PublishInput{RepoID: repo, Commit: commit, Path: "first.bin", SHA256: "gc", Size: 3, StorageKey: key, ContentType: "x", ObjectStored: true}
	if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Nothing uses the blob any more and its grace period is over.
	var blobID string
	if err := store.SQL.QueryRow(`SELECT id FROM blobs WHERE sha256='gc'`).Scan(&blobID); err != nil {
		t.Fatal(err)
	}
	store.SQL.Exec(`DELETE FROM artifacts WHERE blob_id=$1`, blobID)
	store.SQL.Exec(`UPDATE blobs SET orphaned_at=now()-interval '2 hours' WHERE id=$1`, blobID)

	// The collector holds the row and is busy deleting the object.
	deleting, release := make(chan struct{}), make(chan struct{})
	collected := make(chan error, 1)
	go func() {
		_, err := store.CollectOrphanedBlob(ctx, blobID, time.Hour, func(context.Context, string) error {
			close(deleting)
			<-release
			objectsMu.Lock()
			delete(objects, key)
			objectsMu.Unlock()
			return nil
		})
		collected <- err
	}()
	<-deleting

	// The fill looks, sees the row, and so does not upload.
	if exists, err := store.BlobExists(ctx, "gc"); err != nil || !exists {
		t.Fatalf("the blob should still be visible to the fill: %v %v", exists, err)
	}
	in.Path = "again.bin"
	in.ObjectStored = false
	published := make(chan error, 1)
	go func() {
		published <- store.PublishArtifact(ctx, in, func(_ context.Context, k string) error {
			objectsMu.Lock()
			defer objectsMu.Unlock()
			objects[k] = true // what the real callback does: put the object back when it is missing
			return nil
		})
	}()
	// Wait until the publication is queued behind the collector's lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		store.SQL.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`).Scan(&waiting)
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the publication never waited for the collector")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if err := <-collected; err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := store.FindArtifact(ctx, repo, commit, "again.bin"); err != nil {
		t.Fatalf("the artifact was not published: %v", err)
	}
	if !stored() {
		t.Fatal("the artifact is in the catalog but its object was deleted by the collector and never restored")
	}
}

func TestPublishChecksTheObjectOnlyWhenTheCallerDidNotStoreIt(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "chk", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/chk", "public", owner)
	commit := "0123456789abcdef0123456789abcdef01234567"
	calls := 0
	check := func(context.Context, string) error { calls++; return nil }
	in := PublishInput{RepoID: repo, Commit: commit, Path: "a", SHA256: "c1", Size: 1, StorageKey: "blobs/c1", ContentType: "x", ObjectStored: true}
	if err := store.PublishArtifact(ctx, in, check); err != nil || calls != 0 {
		t.Fatalf("a new blob the caller uploaded needs no check: err=%v calls=%d", err, calls)
	}
	in.Path, in.SHA256, in.StorageKey, in.ObjectStored = "b", "c2", "blobs/c2", false
	if err := store.PublishArtifact(ctx, in, check); err != nil || calls != 1 {
		t.Fatalf("a new blob the caller did not upload must be checked: err=%v calls=%d", err, calls)
	}
}

// A purge and a fill publishing the same blob must take the blob's lock before
// the artifact's, or PostgreSQL kills one of them with a deadlock (40P01).
func TestPurgeDoesNotDeadlockWithAPublicationOfTheSameBlob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "purge", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/purge", "public", owner)
	commit := "0123456789abcdef0123456789abcdef01234567"
	in := PublishInput{RepoID: repo, Commit: commit, Path: "f.bin", SHA256: "purge", Size: 3, StorageKey: "blobs/purge", ContentType: "x", ObjectStored: true}
	if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var artifactID, blobID string
	if err := store.SQL.QueryRow(`SELECT id,blob_id FROM artifacts WHERE path='f.bin'`).Scan(&artifactID, &blobID); err != nil {
		t.Fatal(err)
	}

	// The publication: it holds the blob, as PublishArtifact does first.
	tx, err := store.SQL.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM blobs WHERE id=$1 FOR UPDATE`, blobID); err != nil {
		t.Fatal(err)
	}
	purged := make(chan error, 1)
	go func() { purged <- store.PurgeArtifact(ctx, artifactID) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		store.SQL.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`).Scan(&waiting)
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the purge never waited for the blob")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// It then updates the artifact, as the publication's upsert does. With the
	// purge holding the artifact first, this is the deadlock.
	if _, err := tx.ExecContext(ctx, `UPDATE artifacts SET etag='new' WHERE id=$1`, artifactID); err != nil {
		t.Fatalf("the publication was killed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-purged; err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := store.FindArtifact(ctx, repo, commit, "f.bin"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the artifact should be purged: %v", err)
	}
	if err := store.PurgeArtifact(ctx, artifactID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purging again = %v, want ErrNotFound", err)
	}
}

// The cleanup of stale resumptions must stay away from a file being filled.
func TestCleanupOfStaleResumptionsRespectsTheFillLock(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "stale", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/stale", "public", owner)
	r := &Resumption{ID: NewID(), RepositoryID: repo, CommitSHA: "c", Path: "f", ETag: "e", TotalSize: 100, ContentType: "x", ChunkSize: 50}
	if err := store.CreateResumption(ctx, r); err != nil {
		t.Fatal(err)
	}
	store.SQL.Exec(`UPDATE download_resumptions SET updated_at=now()-interval '9 days' WHERE id=$1`, r.ID)
	stale, _ := store.StaleResumptions(ctx, time.Hour, 10)
	if len(stale) != 1 {
		t.Fatalf("stale = %v", stale)
	}
	unlock, err := store.LockResume(ctx, repo, "c", "f")
	if err != nil {
		t.Fatal(err)
	}
	discarded := false
	discard := func(context.Context, *Resumption) error { discarded = true; return nil }
	if removed, err := store.CleanupStaleResumption(ctx, stale[0], time.Hour, discard); removed || err != nil || discarded {
		t.Fatalf("cleanup during a fill = %v %v discarded=%v, want it to leave the state alone", removed, err, discarded)
	}
	unlock()
	if removed, err := store.CleanupStaleResumption(ctx, stale[0], time.Hour, discard); !removed || err != nil {
		t.Fatalf("cleanup after the fill = %v %v", removed, err)
	}
}

// The hit and miss totals of the console are kept as downloads finish, so
// showing them never scans the downloads table.
func TestDownloadCountsFollowFinishedDownloads(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "counts", "", "", "h", "admin")
	repo, _ := store.UpsertRepository(ctx, "models", "org/counts", "public", owner)
	commit := "0123456789abcdef0123456789abcdef01234567"
	totals := func() (int64, int64) {
		t.Helper()
		stats, err := store.CacheStats(ctx, "admin", owner)
		if err != nil {
			t.Fatal(err)
		}
		return stats.Hits, stats.Misses
	}

	if err := store.RecordDownloads(ctx, []DownloadEvent{
		{RepoID: repo, Commit: commit, Path: "a", Status: "hit"},
		{RepoID: repo, Commit: commit, Path: "b", Status: "hit"},
		{RepoID: repo, Commit: commit, Path: "c", Status: "failed", ErrorClass: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	if hits, misses := totals(); hits != 2 || misses != 0 {
		t.Fatalf("after two hits and a failure: %d hits, %d misses", hits, misses)
	}

	for _, tc := range []struct{ errorClass string }{{""}, {"upstream"}} {
		id := NewID()
		if err := store.StartDownload(ctx, id, repo, commit, "fill", Caller{}); err != nil {
			t.Fatal(err)
		}
		_, before := totals()
		defer func() {
			if _, after := totals(); after > before+1 {
				t.Errorf("one download added %d misses", after-before)
			}
		}()
		if err := store.FinishDownload(ctx, id, 10, tc.errorClass); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishDownload(ctx, id, 10, tc.errorClass); err != nil { // closing twice
			t.Fatal(err)
		}
	}
	if hits, misses := totals(); hits != 2 || misses != 1 {
		t.Fatalf("after one finished fill and one failed: %d hits, %d misses, want 2 and 1", hits, misses)
	}

	// Pruning old download records does not erase the totals.
	if _, err := store.SQL.Exec(`DELETE FROM downloads`); err != nil {
		t.Fatal(err)
	}
	if hits, misses := totals(); hits != 2 || misses != 1 {
		t.Fatalf("after retention removed the records: %d hits, %d misses", hits, misses)
	}
}
