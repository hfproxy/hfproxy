package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssue95LoginStylesheetIsServed(t *testing.T) {
	if !strings.Contains(loginPage, `href="/assets/app.css"`) {
		t.Skip("login page no longer links to /assets/app.css")
	}
	rec := httptest.NewRecorder()
	(&Server{}).css(rec, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.css: status=%d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") || rec.Body.Len() == 0 {
		t.Fatalf("stylesheet response: content-type=%q body bytes=%d", rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}
