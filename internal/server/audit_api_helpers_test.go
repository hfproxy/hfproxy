package server

// Shared helpers for the audit_api_* tests (operations API / config / database audit).
// All identifiers are prefixed with auditAPI to avoid collisions with other audit files.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
)

const auditAPISessionSecret = "audit-api-session-secret-0123456789abcdef"

func auditAPIConfig() config.Config {
	return config.Config{
		ListenAddr: "127.0.0.1:0", LogLevel: "info", DatabaseURL: "postgres://unused",
		HFBaseURL: "https://huggingface.co", HFAllowedHosts: []string{"huggingface.co", "*.hf.co"}, HFTimeout: 5 * time.Second,
		S3Region: "us-east-1", S3Bucket: "bucket", S3Prefix: "hfproxy", S3ForcePathStyle: true, S3ServerSideEncryption: true,
		AuthMode: "local", SessionSecret: auditAPISessionSecret, SessionIdleTTL: time.Hour, SessionAbsoluteTTL: 2 * time.Hour,
		CacheMaxFileSize: 1 << 30, CacheRefTTL: time.Minute, CacheNegativeTTL: time.Second, CacheGCGrace: time.Hour,
		LDAPUserFilter: "(uid={username})",
	}
}

func auditAPINewServer(t *testing.T, store *db.Store, cfg config.Config) *Server {
	t.Helper()
	s := &Server{db: store, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), preloads: make(map[string]*preloadJob), preloadSlots: make(chan struct{}, 2)}
	s.runtime.Store(&runtimeState{config: cfg, httpClient: newHFHTTPClient(cfg)})
	return s
}

// auditAPIFreshStore creates an isolated, migrated PostgreSQL database for one test.
func auditAPIFreshStore(t *testing.T) *db.Store {
	t.Helper()
	dsn := auditAPIFreshDSN(t)
	ctx := context.Background()
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	t.Cleanup(func() { store.SQL.Close() })
	if err := db.Migrate(ctx, store.SQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

// auditAPIFreshDSN creates an empty database named audit_api_<random> and returns its DSN.
func auditAPIFreshDSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	name := "audit_api_" + strings.ReplaceAll(db.NewID(), "-", "")[:20]
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
		admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// auditAPICreateUser inserts a user and a real hfproxy bearer token; it returns the user ID and token.
func auditAPICreateUser(t *testing.T, store *db.Store, username, role string) (string, string) {
	t.Helper()
	id := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,$3)`, id, username, role); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	token := "hfpx_audit_" + strings.ReplaceAll(db.NewID(), "-", "")
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name) VALUES($1,$2,$3,$4,'audit')`, db.NewID(), id, HashToken(token, auditAPISessionSecret), token[:10]); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	return id, token
}

// auditAPIDo sends a request through the full handler chain with a bearer token (real DB token).
func auditAPIDo(s *Server, method, target, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// auditAPIDoAs sends a request with an in-process principal (no database needed for authentication).
// A bearer header is still set so the browser CSRF check is skipped exactly as for API clients.
func auditAPIDoAs(s *Server, p *Principal, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), localPrincipalContextKey{}, p))
	req.Header.Set("Authorization", "Bearer audit-api-placeholder-token-000000")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func auditAPIAdmin() *Principal {
	return &Principal{ID: "audit-admin", Username: "admin", Role: "admin", Authenticated: true}
}

// auditAPISeedArtifact inserts a repository (if needed), a blob and an artifact; it returns the repository ID.
func auditAPISeedArtifact(t *testing.T, store *db.Store, repoType, repoID, policy, path string) string {
	t.Helper()
	var repo string
	err := store.SQL.QueryRow(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,$2,$3,$4)
		ON CONFLICT(repo_type,repo_id) DO UPDATE SET access_policy=EXCLUDED.access_policy RETURNING id`, db.NewID(), repoType, repoID, policy).Scan(&repo)
	if err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	blob := db.NewID()
	sha := strings.ReplaceAll(db.NewID()+db.NewID(), "-", "")
	if _, err := store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES($1,$2,10,$3)`, blob, sha, "blobs/"+sha); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	if _, err := store.SQL.Exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id) VALUES($1,$2,$3,$4,$5)`, db.NewID(), repo, strings.Repeat("a", 40), path, blob); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	return repo
}

var _ = http.MethodGet
