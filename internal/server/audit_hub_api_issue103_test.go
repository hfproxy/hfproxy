package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssue103PaginationLinkUsesPublicURLPrefix(t *testing.T) {
	req := httptest.NewRequest("GET", "http://proxy.local/api/models/org/repo/tree/main", nil)
	link := `<https://huggingface.co/api/models/org/repo/tree/main?cursor=abc>; rel="next"`
	got := rewritePaginationLink(link, req, "https://hub.example.com/hf")
	wantPrefix := `<https://hub.example.com/hf/api/models/org/repo/tree/main?cursor=abc>`
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("rewritten Link = %q, want prefix %q", got, wantPrefix)
	}
}
