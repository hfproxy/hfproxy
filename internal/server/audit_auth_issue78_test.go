package server

import (
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
)

func TestIssue78CSRFAndSessionCookiesShareAbsoluteLifetime(t *testing.T) {
	cfg := config.Config{
		SessionIdleTTL:     8 * time.Hour,
		SessionAbsoluteTTL: 24 * time.Hour,
		PublicURL:          "https://proxy.example",
	}
	cookies := newSessionCookies("session-token", "csrf-token", cfg)
	if len(cookies) != 2 {
		t.Fatalf("cookie count = %d, want 2", len(cookies))
	}
	if cookies[0].MaxAge != 24*60*60 || cookies[1].MaxAge != cookies[0].MaxAge {
		t.Fatalf("session MaxAge=%d, CSRF MaxAge=%d, want both equal to absolute TTL", cookies[0].MaxAge, cookies[1].MaxAge)
	}
	if !cookies[0].Secure || !cookies[1].Secure {
		t.Fatal("cookies for HTTPS public URL must be Secure")
	}
}
