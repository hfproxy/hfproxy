package server

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestIssue28HeadPreservesLFSIdentityOnMissAndHit(t *testing.T) {
	content := bytes.Repeat([]byte("model-weights"), 257)
	sum := sha256.Sum256(content)
	digest := fmt.Sprintf("%x", sum)
	cdnETag := `"cdn-multipart-etag-3"`
	var repoID string
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "huggingface.co") {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/issue28/model.safetensors")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.Host != "cdn-lfs.hf.co" || !strings.HasSuffix(r.URL.Path, "/model.safetensors") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", cdnETag)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == http.MethodGet {
			_, _ = w.Write(content)
		}
	})
	env := ahubNewEnv(t, hub)
	repoID = env.repoID
	url := env.proxy.URL + "/" + repoID + "/resolve/main/model.safetensors"
	checkIdentity := func(stage string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodHead, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s HEAD: %v", stage, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s HEAD status = %d", stage, resp.StatusCode)
		}
		got := lfsSHA256(resp.Header.Get("X-Linked-Etag"))
		if got != digest {
			t.Errorf("%s X-Linked-Etag = %q, want LFS SHA-256 %q (CDN ETag %q)", stage, resp.Header.Get("X-Linked-Etag"), digest, resp.Header.Get("ETag"))
		}
		if gotSize := resp.Header.Get("X-Linked-Size"); gotSize != strconv.Itoa(len(content)) {
			t.Errorf("%s X-Linked-Size = %q, want %d", stage, gotSize, len(content))
		}
		if resp.Header.Get("ETag") != `"`+digest+`"` {
			t.Errorf("%s ETag = %q, want linked digest", stage, resp.Header.Get("ETag"))
		}
	}

	checkIdentity("cache miss")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("populate cache status = %d", resp.StatusCode)
	}
	checkIdentity("cache hit")
}
