package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

// admissionHub serves one LFS file through the usual redirect to a CDN host and
// counts the GETs of its body.
func admissionHub(content []byte, digest string, gets *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "huggingface.co" {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/admission/model.bin")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.Host != "cdn-lfs.hf.co" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"admission-cdn"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if r.Method == http.MethodGet {
			gets.Add(1)
			_, _ = w.Write(content)
		}
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (env *ahubEnv) getModel(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	response, err := http.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/" + path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

func (env *ahubEnv) cachedFiles(t *testing.T) int {
	t.Helper()
	var n int
	if err := env.store.SQL.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE repository_id=$1`, env.repoPK).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAFileEntersTheCacheOnItsSecondRequest(t *testing.T) {
	content := []byte("admission-" + ahubRandHex(2000))
	var gets atomic.Int32
	env := ahubNewEnv(t, admissionHub(content, sha256Hex(content), &gets))
	cfg := env.server.currentConfig()
	cfg.CacheEviction.Admission = config.CacheAdmission{Policy: config.AdmitSecondRequest, MinSize: 1}
	env.server.runtime.Load().config = cfg

	first, body := env.getModel(t, "model.bin")
	if first.StatusCode != 200 || string(body) != string(content) || first.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("first request: %d, X-Cache %q, %d bytes", first.StatusCode, first.Header.Get("X-Cache"), len(body))
	}
	if n := env.cachedFiles(t); n != 0 {
		t.Fatalf("the first request stored the file (%d entries)", n)
	}
	second, body := env.getModel(t, "model.bin")
	if second.StatusCode != 200 || string(body) != string(content) || second.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("second request: %d, X-Cache %q", second.StatusCode, second.Header.Get("X-Cache"))
	}
	if n := env.cachedFiles(t); n != 1 {
		t.Fatalf("the second request did not store the file (%d entries)", n)
	}
	third, _ := env.getModel(t, "model.bin")
	if third.Header.Get("X-Cache") != "HIT" || gets.Load() != 2 {
		t.Fatalf("third request: X-Cache %q, %d fetches from the Hub; want a HIT after 2", third.Header.Get("X-Cache"), gets.Load())
	}
}

func TestSmallFilesAndPreloadsAreAlwaysAdmitted(t *testing.T) {
	content := []byte("small-" + ahubRandHex(100))
	var gets atomic.Int32
	env := ahubNewEnv(t, admissionHub(content, sha256Hex(content), &gets))
	cfg := env.server.currentConfig()
	cfg.CacheEviction.Admission = config.CacheAdmission{Policy: config.AdmitSecondRequest, MinSize: 1 << 20}
	env.server.runtime.Load().config = cfg
	if response, _ := env.getModel(t, "model.bin"); response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if n := env.cachedFiles(t); n != 1 {
		t.Fatalf("a file under the minimum size was not stored (%d entries)", n)
	}
}

func TestAFileOverTheSizeLimitIsStillRefused(t *testing.T) {
	content := []byte("oversized-" + ahubRandHex(2000))
	var gets atomic.Int32
	env := ahubNewEnv(t, admissionHub(content, sha256Hex(content), &gets))
	cfg := env.server.currentConfig()
	cfg.CacheMaxFileSize = 100
	cfg.CacheEviction.Admission = config.CacheAdmission{Policy: config.AdmitSecondRequest, MinSize: 1}
	env.server.runtime.Load().config = cfg
	if response, _ := env.getModel(t, "model.bin"); response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413 as without admission", response.StatusCode)
	}
}

func TestFrequencyAdmissionComparesWithTheNextVictim(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 100
	cfg.CacheEviction.Admission = config.CacheAdmission{Policy: config.AdmitFrequency, MinSize: 1}
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/admission")
	seedScored(t, store, repo, "victim", 90, "2 days", 3, 1)
	ctx := context.Background()
	candidate := &fileResolution{repoPK: repo, commit: pinCommitB, sizeKnown: true}
	candidate.parsed.FilePath = "candidate"
	candidate.meta.Size = 50

	// The first request for a file loses to a file used three times.
	if admitted, reason := s.admissionDecision(ctx, candidate); admitted {
		t.Fatalf("admitted on first sight (%s)", reason)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.SQL.Exec(`INSERT INTO downloads(id,repository_id,commit_sha,path,status,bytes) VALUES($1,$2,$3,'candidate','miss',0)`, db.NewID(), repo, pinCommitB); err != nil {
			t.Fatal(err)
		}
	}
	// Three earlier requests plus this one beat the victim's three uses.
	if admitted, reason := s.admissionDecision(ctx, candidate); !admitted {
		t.Fatalf("refused after three requests (%s)", reason)
	}
	// With room left, nothing is compared.
	candidate.meta.Size = 5
	if _, err := store.SQL.Exec(`DELETE FROM downloads`); err != nil {
		t.Fatal(err)
	}
	if admitted, reason := s.admissionDecision(ctx, candidate); !admitted || reason != "room" {
		t.Fatalf("refused while the cache has room (%s)", reason)
	}
}
