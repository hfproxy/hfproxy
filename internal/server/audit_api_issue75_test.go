package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nicolas/hfproxy/internal/db"
)

func TestIssue75TestHubSendsExplicitTokenToNewOrigin(t *testing.T) {
	var mu sync.Mutex
	var authorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorization = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"name":"test-user"}`))
	}))
	defer target.Close()

	cfg := auditAPIConfig()
	cfg.HFToken = "stored-token"
	s := auditAPINewServer(t, &db.Store{}, cfg)
	p := &Principal{ID: "admin", Role: "admin", Authenticated: true, TokenID: "token", Scopes: []string{"admin"}}
	input, err := json.Marshal(map[string]string{"hf_base_url": target.URL, "hf_token": "typed-for-new-hub"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/test-hub", bytes.NewReader(input))
	req = req.WithContext(context.WithValue(req.Context(), localPrincipalContextKey{}, p))
	req.Header.Set("Authorization", "Bearer local-placeholder-token-000000")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("test-hub status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if authorization != "Bearer typed-for-new-hub" {
		t.Fatalf("upstream Authorization=%q, want explicit token", authorization)
	}
}

func TestIssue75StoredTokenReuseRequiresSameOrigin(t *testing.T) {
	for _, tt := range []struct {
		first, second string
		want          bool
	}{
		{"https://hub.example", "https://hub.example:443", true},
		{"https://hub.example", "http://hub.example", false},
		{"https://hub.example", "https://hub.example:8443", false},
		{"https://hub.example", "https://other.example", false},
	} {
		if got := sameURLOrigin(tt.first, tt.second); got != tt.want {
			t.Errorf("sameURLOrigin(%q, %q)=%v, want %v", tt.first, tt.second, got, tt.want)
		}
	}
}
