package server

// Shared helpers for the audit_hub_* tests (Hub protocol compatibility audit).
// All identifiers use the "ahub" prefix to avoid colliding with other audit files.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/storage"
)

func ahubRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ahubFakeS3 is a tiny in-memory, path-style S3 endpoint good enough for the
// AWS SDK calls hfproxy performs (PutObject, GetObject with Range, HeadObject,
// DeleteObject, HeadBucket). It understands aws-chunked uploads.
type ahubFakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *ahubFakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") || strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING") {
			body = ahubDecodeAWSChunked(body)
		}
		f.mu.Lock()
		f.objects[key] = body
		f.mu.Unlock()
		w.Header().Set("ETag", `"`+ahubRandHex(8)+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		f.mu.Lock()
		data, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			if strings.Count(key, "/") == 0 { // HeadBucket
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		http.ServeContent(w, r, "", time.Unix(0, 0), bytes.NewReader(data))
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func ahubDecodeAWSChunked(body []byte) []byte {
	var out bytes.Buffer
	rd := bufio.NewReader(bytes.NewReader(body))
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		sizeHex, _, _ := strings.Cut(line, ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil || size == 0 {
			break
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(rd, chunk); err != nil {
			break
		}
		out.Write(chunk)
		_, _ = rd.ReadString('\n')
	}
	return out.Bytes()
}

func ahubNewFakeS3(t *testing.T) (*storage.S3, *ahubFakeS3) {
	t.Helper()
	fake := &ahubFakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	st, err := storage.New(context.Background(), srv.URL, "us-east-1", "bucket", "", "ak", "sk", true, false)
	if err != nil {
		t.Fatalf("fake s3: %v", err)
	}
	return st, fake
}

// ahubRewriteTransport sends every upstream request (whatever its scheme/host)
// to the fake Hub server, preserving the requested host in req.Host so the fake
// can emulate huggingface.co and CDN hosts. hfproxy's own redirect/host checks
// in fetchHub still run before the transport is reached.
type ahubRewriteTransport struct{ target string }

func (rt ahubRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Host = req.URL.Host
	clone.URL.Scheme = "http"
	clone.URL.Host = rt.target
	return http.DefaultTransport.RoundTrip(clone)
}

type ahubEnv struct {
	server *Server
	store  *db.Store
	proxy  *httptest.Server
	hub    *httptest.Server
	repoID string
	repoPK string
	fakeS3 *ahubFakeS3
}

func ahubConfig() config.Config {
	return config.Config{
		ListenAddr: "127.0.0.1:0", HFBaseURL: "https://huggingface.co", HFTimeout: 30 * time.Second,
		HFAllowedHosts: []string{"huggingface.co", "hf.co", "*.huggingface.co", "*.hf.co", "*.xethub.hf.co"},
		S3Region:       "us-east-1", S3Bucket: "bucket", S3ForcePathStyle: true,
		AuthMode: "local", AnonymousPublicRead: true, SessionSecret: "audit-hub-session-secret-0123456789abcdef",
		SessionIdleTTL: time.Hour, SessionAbsoluteTTL: time.Hour, CacheMaxFileSize: 1 << 30,
		CacheRefTTL: time.Minute, CacheNegativeTTL: 30 * time.Second, CacheGCGrace: 24 * time.Hour,
	}
}

// ahubNewEnv builds a DB-backed hfproxy with a fake Hub (hub handler) and fake S3,
// and registers a public model repository "audit-hub/<random>".
func ahubNewEnv(t *testing.T, hub http.Handler) *ahubEnv {
	t.Helper()
	dsn := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { store.SQL.Close() })
	if err := db.Migrate(ctx, store.SQL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	objects, fake := ahubNewFakeS3(t)
	cfg := ahubConfig()
	s, err := New(ctx, cfg, store, objects, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	hubServer := httptest.NewServer(hub)
	t.Cleanup(hubServer.Close)
	client := &http.Client{
		Timeout:       cfg.HFTimeout,
		Transport:     ahubRewriteTransport{target: hubServer.Listener.Addr().String()},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.runtime.Store(&runtimeState{config: cfg, storage: objects, httpClient: client})

	repoPK := db.NewID()
	repoID := "audit-hub/m" + ahubRandHex(5)
	if _, err := store.SQL.ExecContext(ctx, `INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models',$2,'public')`, repoPK, repoID); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	proxy := httptest.NewServer(s.Handler())
	t.Cleanup(proxy.Close)
	return &ahubEnv{server: s, store: store, proxy: proxy, hub: hubServer, repoID: repoID, repoPK: repoPK, fakeS3: fake}
}

const ahubCommit = "0123456789abcdef0123456789abcdef01234567"
