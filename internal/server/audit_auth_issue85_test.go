package server

import (
	"fmt"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestIssue85LoginRateLimitUsesForwardedClientOnlyFromTrustedProxy(t *testing.T) {
	cfg := auditAuthConfig()
	cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	s := newAuditAuthServerNoDB(cfg)
	for i := 1; i <= 10; i++ {
		req := httptest.NewRequest("POST", "/login", nil)
		req.RemoteAddr = "10.0.0.2:40000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i))
		if retry, ok := s.allowLoginAttempt(req, fmt.Sprintf("unknown%d", i)); !ok {
			t.Fatalf("client %d behind shared proxy was rate limited (retry after %ds)", i, retry)
		}
	}
	req := httptest.NewRequest("POST", "/login", nil)
	req.RemoteAddr = "10.0.0.2:40000"
	req.Header.Set("X-Forwarded-For", "192.0.2.50")
	if retry, ok := s.allowLoginAttempt(req, "alice"); !ok {
		t.Fatalf("alice was rate limited behind shared proxy (retry after %ds)", retry)
	}

	// An untrusted direct peer cannot rotate the rate-limit identity by forging XFF.
	for i := 0; i < 10; i++ {
		spoofed := httptest.NewRequest("POST", "/login", nil)
		spoofed.RemoteAddr = "198.51.100.10:40000"
		spoofed.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i+1))
		if got := s.loginClientIP(spoofed); got != "198.51.100.10" {
			t.Fatalf("untrusted X-Forwarded-For produced client IP %q", got)
		}
	}
}

func TestIssue85LoginClientIPWithoutRuntimeUsesRemoteAddress(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("POST", "/login", nil)
	req.RemoteAddr = "198.51.100.10:40000"
	req.Header.Set("X-Forwarded-For", "192.0.2.50")

	if got := s.loginClientIP(req); got != "198.51.100.10" {
		t.Fatalf("client IP = %q, want remote address when runtime is unavailable", got)
	}
}
