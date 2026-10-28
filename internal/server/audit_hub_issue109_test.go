package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIssue109ConcurrentInstancesShareResumeLock(t *testing.T) {
	content := make([]byte, 32<<10)
	for i := range content {
		content[i] = byte(i * 7)
	}
	const etag = `"issue109-shard"`
	fileName := "shard.bin"
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "huggingface.co" || !strings.HasSuffix(r.URL.Path, "/resolve/main/"+fileName) && !strings.HasSuffix(r.URL.Path, "/resolve/"+ahubCommit+"/"+fileName) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Repo-Commit", ahubCommit)
		w.Header().Set("ETag", etag)
		if r.Method == http.MethodGet {
			time.Sleep(100 * time.Millisecond)
		}
		http.ServeContent(w, r, fileName, time.Unix(1700000000, 0), bytes.NewReader(content))
	})
	env := ahubNewEnv(t, hub)
	second, err := New(context.Background(), ahubConfig(), env.store, env.server.currentStorage(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	second.runtime.Store(env.server.runtime.Load())
	proxyB := httptest.NewServer(second.Handler())
	t.Cleanup(proxyB.Close)

	type result struct {
		status int
		body   []byte
		err    error
	}
	fetch := func(base string, out chan<- result) {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(base + "/" + env.repoID + "/resolve/main/" + fileName)
		if err != nil {
			out <- result{err: err}
			return
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		out <- result{status: resp.StatusCode, body: body, err: readErr}
	}
	a, b := make(chan result, 1), make(chan result, 1)
	go fetch(env.proxy.URL, a)
	time.Sleep(25 * time.Millisecond)
	go fetch(proxyB.URL, b)
	for name, got := range map[string]result{"instance A": <-a, "instance B": <-b} {
		if got.err != nil || got.status != http.StatusOK || !bytes.Equal(got.body, content) {
			t.Errorf("%s response status=%d bytes=%d err=%v; want complete %d-byte response", name, got.status, len(got.body), got.err, len(content))
		}
	}
}
