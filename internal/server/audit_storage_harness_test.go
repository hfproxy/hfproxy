package server

// Shared harness for the storage/download-pipeline audit tests (audit_storage_*_test.go).
//
// The tests need a real PostgreSQL and a real S3-compatible store (Garage or MinIO):
//
//	HFPROXY_TEST_DATABASE_URL   postgres DSN (database is migrated by the harness)
//	HFPROXY_TEST_S3_ENDPOINT    e.g. http://127.0.0.1:59000
//	HFPROXY_TEST_S3_ACCESS_KEY / HFPROXY_TEST_S3_SECRET_KEY
//	HFPROXY_TEST_S3_BUCKET      (default "hfproxy")
//	HFPROXY_TEST_S3_REGION      (default "garage")
//
// Tests are skipped when the variables are unset. The fake Hugging Face Hub is an
// httptest server; the production HTTP client (newHFHTTPClient) is used unchanged except
// that its dialer is pointed at the fake Hub, so "http://localhost" passes fetchHub's
// loopback/port checks.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/storage"
)

const auditStorageCommit = "0123456789abcdef0123456789abcdef01234567"

type auditStorageEnv struct {
	server   *Server
	store    *db.Store
	objects  *storage.S3
	repoPK   string
	repoID   string
	hub      *httptest.Server
	s3Target string
}

type auditStorageOptions struct {
	s3Endpoint string // override (e.g. an intercepting proxy in front of the real S3)
	mutate     func(*config.Config)
}

func auditStorageS3Endpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("HFPROXY_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("HFPROXY_TEST_S3_ENDPOINT is not set")
	}
	return endpoint
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func auditRandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func auditSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newAuditStorageEnv(t *testing.T, hub http.Handler, opts auditStorageOptions) *auditStorageEnv {
	t.Helper()
	dsn := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}
	s3Endpoint := auditStorageS3Endpoint(t)
	if opts.s3Endpoint != "" {
		s3Endpoint = opts.s3Endpoint
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := db.Open(ctx, dsn) // production pool settings (MaxOpenConns=20)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { store.SQL.Close() })
	if err := db.Migrate(ctx, store.SQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	hubServer := httptest.NewServer(hub)
	t.Cleanup(hubServer.Close)

	cfg := config.Config{
		ListenAddr: "127.0.0.1:0", HFBaseURL: "http://localhost", HFTimeout: 30 * time.Second,
		S3Endpoint: s3Endpoint, S3Region: envOr("HFPROXY_TEST_S3_REGION", "garage"), S3Bucket: envOr("HFPROXY_TEST_S3_BUCKET", "hfproxy"),
		S3Prefix: "audit-storage-" + auditRandomHex(4), S3ForcePathStyle: true,
		S3AccessKey: os.Getenv("HFPROXY_TEST_S3_ACCESS_KEY"), S3SecretKey: os.Getenv("HFPROXY_TEST_S3_SECRET_KEY"),
		AuthMode: "local", AnonymousPublicRead: true, SessionSecret: "audit-storage-session-secret-0123456789",
		SessionIdleTTL: time.Hour, SessionAbsoluteTTL: time.Hour, CacheMaxFileSize: 1 << 30,
		CacheRefTTL: time.Minute, CacheGCGrace: 24 * time.Hour,
	}
	if opts.mutate != nil {
		opts.mutate(&cfg)
	}
	objects, err := storage.New(ctx, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3AccessKey, cfg.S3SecretKey, true, false)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(ctx, cfg, store, objects, logger)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// Production client, but dial the fake Hub regardless of the requested address.
	client := newHFHTTPClient(cfg)
	transport := client.Transport.(*http.Transport)
	hubAddr := hubServer.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, hubAddr)
	}
	s.runtime.Store(&runtimeState{config: cfg, storage: objects, httpClient: client})

	repoPK := db.NewID()
	repoID := "audit/storage-" + auditRandomHex(6)
	if _, err := store.SQL.ExecContext(ctx, `INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models',$2,'public')`, repoPK, repoID); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	return &auditStorageEnv{server: s, store: store, objects: objects, repoPK: repoPK, repoID: repoID, hub: hubServer}
}

// get performs an anonymous GET through the real handler chain entry point for resolve.
func (e *auditStorageEnv) get(ctx context.Context, file string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/"+e.repoID+"/resolve/"+auditStorageCommit+"/"+file, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	e.server.resolveFile(rec, req)
	return rec
}

func (e *auditStorageEnv) artifactCount(t *testing.T, file string) int {
	t.Helper()
	var n int
	if err := e.store.SQL.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE repository_id=$1 AND path=$2`, e.repoPK, file).Scan(&n); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	return n
}

// auditHubFile is a minimal model of the Hub resolve endpoint for one LFS file: the
// /resolve/ URL answers 302 with X-Linked-Etag / X-Linked-Size / X-Repo-Commit and the
// CDN URL serves the bytes (with Range support).
type auditHubFile struct {
	name       string
	advertised []byte // bytes whose sha256/size are advertised as the LFS oid
	served     []byte // bytes actually served by the CDN (normally == advertised)
	onHead     func()
	onGet      func(w http.ResponseWriter, r *http.Request) bool // return true if handled
}

func (f *auditHubFile) handler(repoID string) http.Handler {
	mux := http.NewServeMux()
	oid := auditSHA256(f.advertised)
	mux.HandleFunc("/"+repoID+"/resolve/"+auditStorageCommit+"/"+f.name, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && f.onHead != nil {
			f.onHead()
		}
		w.Header().Set("X-Repo-Commit", auditStorageCommit)
		w.Header().Set("X-Linked-Etag", `"`+oid+`"`)
		w.Header().Set("X-Linked-Size", itoa(int64(len(f.advertised))))
		w.Header().Set("ETag", `"`+oid+`"`)
		w.Header().Set("Location", "/cdn/"+oid)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/cdn/"+oid, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && f.onGet != nil && f.onGet(w, r) {
			return
		}
		w.Header().Set("ETag", `"cdn-`+oid[:16]+`"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, f.name, time.Unix(1700000000, 0), bytesReadSeeker(f.served))
	})
	return mux
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func bytesReadSeeker(b []byte) io.ReadSeeker { return bytes.NewReader(b) }
