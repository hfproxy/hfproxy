package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCanStreamCacheMissRequiresNoIntegrityValidator(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/model.bin", nil)
	if !shouldStreamCacheMiss(req, "") {
		t.Fatal("ordinary full GET should be streamed")
	}
	if shouldStreamCacheMiss(req, strings.Repeat("a", 64)) {
		t.Fatal("LFS response with an advertised digest must be verified before streaming")
	}
	req.Header.Set("Range", "bytes=10-")
	if shouldStreamCacheMiss(req, "") {
		t.Fatal("range request must not use the full-response streaming path")
	}
}

func TestIssue99CorruptLFSResponseIsNotCleanHTTP200(t *testing.T) {
	advertised := bytes.Repeat([]byte("weights-block!"), 8192)
	corrupted := bytes.Clone(advertised)
	corrupted[1000] ^= 0xff
	digest := auditSHA256(advertised)
	var repoID string
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "huggingface.co" && (strings.HasSuffix(r.URL.Path, "/resolve/main/model.safetensors") || strings.HasSuffix(r.URL.Path, "/resolve/"+ahubCommit+"/model.safetensors")) {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", fmt.Sprint(len(advertised)))
			w.Header().Set("ETag", `W/"redirect-body"`)
			w.Header().Set("Location", "https://cdn-lfs.hf.co/repos/aa/bb/"+digest)
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.Host == "cdn-lfs.hf.co" && strings.HasSuffix(r.URL.Path, "/"+digest) {
			w.Header().Set("ETag", `"cdn-corrupt"`)
			http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(corrupted))
			return
		}
		http.NotFound(w, r)
	})
	env := ahubNewEnv(t, hub)
	repoID = env.repoID
	resp, err := http.Get(env.proxy.URL + "/" + repoID + "/resolve/main/model.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK && readErr == nil && int64(len(body)) == resp.ContentLength && bytes.Equal(body, corrupted) {
		t.Fatalf("client received a clean 200 with the complete corrupted body (%d bytes)", len(body))
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("corrupted LFS response status = %d, want 502", resp.StatusCode)
	}
	if bytes.Contains(body, corrupted) {
		t.Fatal("client received corrupted LFS bytes")
	}
}
