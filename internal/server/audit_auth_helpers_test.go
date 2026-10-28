package server

// Shared helpers for the audit_auth_* tests. DB-backed tests read the DSN from
// HFPROXY_TEST_DATABASE_URL and are skipped when it is unset.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
)

const auditAuthSecret = "audit-auth-session-secret-0123456789abcdef"

func auditAuthConfig() config.Config {
	return config.Config{
		ListenAddr:         "127.0.0.1:0",
		LogLevel:           "info",
		AuthMode:           "local",
		SessionSecret:      auditAuthSecret,
		SessionIdleTTL:     8 * time.Hour,
		SessionAbsoluteTTL: 24 * time.Hour,
		HFBaseURL:          "http://127.0.0.1:1",
		HFTimeout:          5 * time.Second,
		CacheRefTTL:        time.Minute,
		CacheMaxFileSize:   1 << 30,
		DatabaseURL:        "postgres://unused",
	}
}

func newAuditAuthServerNoDB(cfg config.Config) *Server {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), preloads: map[string]*preloadJob{}, preloadSlots: make(chan struct{}, 2)}
	s.runtime.Store(&runtimeState{config: cfg})
	return s
}

func newAuditAuthServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.SQL.Close() })
	if err := db.Migrate(ctx, store.SQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.SQL.ExecContext(ctx, `TRUNCATE users, external_identities, groups, group_members, access_tokens, repositories, repo_grants, revisions, blobs, artifacts, downloads, audit_events, sessions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	s := newAuditAuthServerNoDB(auditAuthConfig())
	s.db = store
	return s
}

func auditAuthCreateUser(t *testing.T, s *Server, username, role, password string) string {
	t.Helper()
	hash := ""
	if password != "" {
		var err error
		hash, err = HashPassword(password)
		if err != nil {
			t.Fatal(err)
		}
	}
	id := db.NewID()
	if _, err := s.db.SQL.Exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$2,$3,$4)`, id, username, hash, role); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// auditAuthCreateToken mirrors `hfproxy admin token create` (default scopes).
func auditAuthCreateToken(t *testing.T, s *Server, userID string) string {
	t.Helper()
	secret := "hfpx_" + randomSecret(32)
	if _, err := s.db.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name) VALUES($1,$2,$3,$4,'test')`, db.NewID(), userID, HashToken(secret, auditAuthSecret), secret[:16]); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return secret
}

// auditAuthSession creates a browser session and returns its cookies.
func auditAuthSession(t *testing.T, s *Server, userID string) []*http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := s.createSession(context.Background(), userID, rec, req); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return rec.Result().Cookies()
}

func auditAuthDo(s *Server, method, target, body string, cookies []*http.Cookie, header http.Header) *httptest.ResponseRecorder {
	return auditAuthDoRequest(s, method, target, body, cookies, header, "")
}

func auditAuthDoRequest(s *Server, method, target, body string, cookies []*http.Cookie, header http.Header, remoteAddr string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	for k, v := range header {
		req.Header[k] = v
	}
	for _, c := range cookies {
		req.AddCookie(c)
		if c.Name == "hfproxy_csrf" {
			req.Header.Set("X-CSRF-Token", c.Value)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func auditAuthCreateRepo(t *testing.T, s *Server, repoID, policy string) string {
	t.Helper()
	id := db.NewID()
	if _, err := s.db.SQL.Exec(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models',$2,$3)`, id, repoID, policy); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return id
}

func auditAuthCreateArtifact(t *testing.T, s *Server, repoPK, path string) {
	t.Helper()
	blobID := db.NewID()
	sha := strings.Repeat("a", 63) + string("0123456789abcdef"[len(path)%16])
	sha = blobID[:8] + sha[8:]
	if _, err := s.db.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES($1,$2,1234,$3)`, blobID, sha, "k/"+blobID); err != nil {
		t.Fatalf("create blob: %v", err)
	}
	if _, err := s.db.SQL.Exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id) VALUES($1,$2,$3,$4,$5)`, db.NewID(), repoPK, strings.Repeat("c", 40), path, blobID); err != nil {
		t.Fatalf("create artifact: %v", err)
	}
}
