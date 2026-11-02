package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUnsafeFormatByName(t *testing.T) {
	for name, want := range map[string]bool{
		"pytorch_model.bin": true, "model-00001-of-00002.bin": true, "a/b/weights.PT": true, "x.pth": true,
		"m.ckpt": true, "d.pkl": true, "d.pickle": true, "j.joblib": true, "p.pdparams": true,
		"model.safetensors": false, "model.gguf": false, "m.onnx": false, "config.json": false,
		"tokenizer.model": false, "data.npy": false, "data.npz": false, "part.parquet": false, "bin": false, "README.md": false,
	} {
		if got := unsafeFormat(name); got != want {
			t.Errorf("unsafeFormat(%q) = %v, want %v", name, got, want)
		}
	}
}

// safetensorsFile builds a minimal valid safetensors file.
func safetensorsFile(payload int) []byte {
	header := []byte(`{"w":{"dtype":"U8","shape":[` + fmt.Sprint(payload) + `],"data_offsets":[0,` + fmt.Sprint(payload) + `]}}`)
	out := make([]byte, 8, 8+len(header)+payload)
	binary.LittleEndian.PutUint64(out, uint64(len(header)))
	out = append(out, header...)
	return append(out, bytes.Repeat([]byte{1}, payload)...)
}

func TestCheckSafetensors(t *testing.T) {
	read := func(content []byte) func(int64, int64) ([]byte, error) {
		return func(offset, n int64) ([]byte, error) {
			if offset+n > int64(len(content)) {
				return nil, io.ErrUnexpectedEOF
			}
			return content[offset : offset+n], nil
		}
	}
	long := make([]byte, 16)
	binary.LittleEndian.PutUint64(long, 1<<40)
	notJSON := append(make([]byte, 8), []byte("PK\x03\x04junk")...)
	binary.LittleEndian.PutUint64(notJSON, 8)
	for name, tc := range map[string]struct {
		content []byte
		ok      bool
	}{
		"valid":            {safetensorsFile(16), true},
		"empty":            {nil, false},
		"too short":        {[]byte("abc"), false},
		"huge header":      {long, false},
		"zero header":      {make([]byte, 16), false},
		"not JSON":         {notJSON, false},
		"a pickle renamed": {[]byte("\x80\x04\x95\x10\x00\x00\x00\x00\x00\x00\x00}\x94(\x8c\x01a\x94K\x01u."), false},
	} {
		err := checkSafetensors(int64(len(tc.content)), read(tc.content))
		if (err == nil) != tc.ok {
			t.Errorf("%s: error = %v, want ok = %v", name, err, tc.ok)
		}
	}
}

func (s *Server) setSafeFormats(on bool) {
	state := s.runtime.Load()
	next := *state
	next.config.SafeFormatsOnly = on
	s.runtime.Store(&next)
}

func TestSafeFormatsGateWhatIsPublishedAndServed(t *testing.T) {
	env := newPublishEnv(t, nil)
	if status, _, body := env.do(t, http.MethodPost, "/api/repos/create", env.alice, map[string]any{"name": "m", "organization": "alice"}); status != http.StatusOK {
		t.Fatalf("create = %d %s", status, body)
	}
	commit := func(items ...map[string]any) (int, string) {
		status, _, body := env.do(t, http.MethodPost, "/api/models/alice/m/commit/main", env.alice, commitLines("c", items...))
		return status, string(body)
	}
	// With the setting off, anything goes, including a pickle.
	if status, body := commit(inlineFile("legacy.bin", []byte("pickle")), inlineFile("config.json", []byte("{}"))); status != http.StatusOK {
		t.Fatalf("commit with the setting off = %d %s", status, body)
	}

	env.server.setSafeFormats(true)

	// What is already there is no longer served, and looks like a missing file.
	status, header, body := env.do(t, http.MethodGet, "/alice/m/resolve/main/legacy.bin", env.alice, nil)
	if status != http.StatusNotFound || header.Get("X-Error-Code") != "EntryNotFound" || !strings.Contains(string(body), "safetensors") {
		t.Fatalf("serving a pickle = %d %v %s", status, header, body)
	}
	if status, _, _ := env.do(t, http.MethodHead, "/alice/m/resolve/main/legacy.bin", env.alice, nil); status != http.StatusNotFound {
		t.Fatalf("HEAD of a pickle = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/alice/m/resolve/main/config.json", env.alice, nil); status != http.StatusOK {
		t.Fatalf("serving a safe file = %d", status)
	}

	// New pickles are refused, in preupload and in the commit.
	status, _, body = env.do(t, http.MethodPost, "/api/models/alice/m/preupload/main", env.alice, map[string]any{"files": []map[string]any{{"path": "w.pt", "size": 10}}})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(body), "w.pt") {
		t.Fatalf("preupload of a pickle = %d %s", status, body)
	}
	if status, body := commit(inlineFile("w.pth", []byte("pickle"))); status != http.StatusUnprocessableEntity {
		t.Fatalf("committing a pickle = %d %s", status, body)
	}

	// A file named .safetensors has to be one, inline or through LFS.
	if status, body := commit(inlineFile("fake.safetensors", []byte("\x80\x04not safetensors"))); status != http.StatusUnprocessableEntity || !strings.Contains(body, "not a valid safetensors") {
		t.Fatalf("committing a fake safetensors = %d %s", status, body)
	}
	fake := bytes.Repeat([]byte("PK\x03\x04"), 1024)
	env.uploadLFS(t, "alice/m", env.alice, fake)
	if status, body := commit(lfsFile("big.safetensors", fake)); status != http.StatusUnprocessableEntity {
		t.Fatalf("committing a fake LFS safetensors = %d %s", status, body)
	}
	real := safetensorsFile(4096)
	env.uploadLFS(t, "alice/m", env.alice, real)
	if status, body := commit(lfsFile("model.safetensors", real), inlineFile("small.safetensors", safetensorsFile(8))); status != http.StatusOK {
		t.Fatalf("committing real safetensors = %d %s", status, body)
	}

	// An administrator can exempt the repository: it serves and accepts pickles.
	var repoPK string
	if err := env.store.SQL.QueryRow(`SELECT id FROM repositories WHERE repo_id='alice/m'`).Scan(&repoPK); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetAllowUnsafeFormats(t.Context(), repoPK, true); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/alice/m/resolve/main/legacy.bin", env.alice, nil); status != http.StatusOK {
		t.Fatalf("serving a pickle from an exempted repository = %d", status)
	}
	if status, body := commit(inlineFile("w.pt", []byte("pickle"))); status != http.StatusOK {
		t.Fatalf("committing a pickle to an exempted repository = %d %s", status, body)
	}
}

func TestSafeFormatsSkipPreloadedHubFiles(t *testing.T) {
	files := map[string]string{"config.json": `{"a":1}`, "model.safetensors": "weights", "pytorch_model.bin": "pickle"}
	var asked []string
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/main") {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			fmt.Fprint(w, `[{"type":"file","path":"config.json"},{"type":"file","path":"model.safetensors"},{"type":"file","path":"pytorch_model.bin"}]`)
			return
		}
		for name, content := range files {
			if strings.HasSuffix(r.URL.Path, "/"+name) {
				asked = append(asked, name)
				w.Header().Set("X-Repo-Commit", ahubCommit)
				w.Header().Set("ETag", `"`+name+`"`)
				w.Header().Set("Content-Length", fmt.Sprint(len(content)))
				if r.Method == http.MethodGet {
					io.WriteString(w, content)
				}
				return
			}
		}
		http.NotFound(w, r)
	})
	env := ahubNewEnv(t, hub)
	env.server.setSafeFormats(true)
	token := operatorToken(t, env, "preloader")

	req, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/api/v1/repos/"+env.repoPK+"/preload", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	job := waitForPreload(t, env, token)
	if job["status"] != "completed" || job["files_total"].(float64) != 2 || job["files_processed"].(float64) != 2 || job["failed_files"].(float64) != 0 || job["skipped_files"].(float64) != 1 {
		t.Fatalf("preload job = %v", job)
	}
	for _, name := range asked {
		if name == "pytorch_model.bin" {
			t.Fatal("the pickle was requested from the Hub")
		}
	}

	// A client asking for it directly gets a missing file, and the Hub is not asked.
	before := len(asked)
	resp, err = http.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/pytorch_model.bin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "EntryNotFound" || len(asked) != before {
		t.Fatalf("direct request = %d %q, hub asked %d more times", resp.StatusCode, resp.Header.Get("X-Error-Code"), len(asked)-before)
	}

	// With the exemption, it is fetched like any file.
	if err := env.store.SetAllowUnsafeFormats(t.Context(), env.repoPK, true); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/pytorch_model.bin")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(data) != "pickle" {
		t.Fatalf("exempted request = %d %q", resp.StatusCode, data)
	}
}

// The label values of a counter are given as name=value.
func TestRefusedFormatsAreCountedPerPlace(t *testing.T) {
	s := &Server{metrics: newMetrics()}
	s.countRefusedFormat("read")
	s.countRefusedFormat("publish")
	s.countRefusedFormat("publish")
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "hfproxy_unsafe_format_refused_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "where" {
					got[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	if got["read"] != 1 || got["publish"] != 2 || len(got) != 2 {
		t.Fatalf("refusals by place = %v, want read 1 and publish 2", got)
	}
}
