package server

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func issue100SlowHub(content []byte, firstHalfSent *atomic.Bool, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/big.bin") {
			http.NotFound(w, r)
			return
		}
		body := content
		status := http.StatusOK
		if value := r.Header.Get("Range"); strings.HasPrefix(value, "bytes=") {
			var start int
			if _, err := fmt.Sscanf(strings.TrimPrefix(value, "bytes="), "%d-", &start); err == nil && start >= 0 && start < len(content) {
				body = content[start:]
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
				status = http.StatusPartialContent
			}
		}
		w.Header().Set("X-Repo-Commit", ahubCommit)
		w.Header().Set("ETag", `"issue100-big"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(status)
		half := len(body) / 2
		_, _ = w.Write(body[:half])
		w.(http.Flusher).Flush()
		firstHalfSent.Store(true)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(body[half:])
	}
}

func TestIssue100ColdRangeRequestGetsHeadersWhileUpstreamIsStillSending(t *testing.T) {
	content := bytes.Repeat([]byte{'w'}, 4<<20)
	release := make(chan struct{})
	var halfSent atomic.Bool
	env := ahubNewEnv(t, issue100SlowHub(content, &halfSent, release))
	defer close(release)
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 3 * time.Second}}
	req, err := http.NewRequest(http.MethodGet, env.proxy.URL+"/"+env.repoID+"/resolve/main/big.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=1048576-")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("cold range request did not get response headers while upstream was still sending (half sent: %v): %v", halfSent.Load(), err)
	}
	for i := 0; i < 100 && !halfSent.Load(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	_ = resp.Body.Close()
	if !halfSent.Load() {
		t.Fatal("upstream did not reach the mid-transfer pause")
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("cold range status = %d, want 206", resp.StatusCode)
	}
}

func TestIssue100CoalescedFollowerGetsHeadersBeforeLeaderFinishes(t *testing.T) {
	content := bytes.Repeat([]byte{'w'}, 4<<20)
	release := make(chan struct{})
	var halfSent atomic.Bool
	env := ahubNewEnv(t, issue100SlowHub(content, &halfSent, release))
	defer close(release)
	leader, err := http.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/big.bin")
	if err != nil {
		t.Fatalf("leader request: %v", err)
	}
	defer leader.Body.Close()
	for i := 0; i < 100 && !halfSent.Load(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !halfSent.Load() {
		t.Fatal("upstream did not start sending the leader response")
	}
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 3 * time.Second}}
	resp, err := client.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/big.bin")
	if err != nil {
		t.Fatalf("coalesced follower did not get response headers while the leader was filling cache: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follower status = %d, want 200", resp.StatusCode)
	}
}
