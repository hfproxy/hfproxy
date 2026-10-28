package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssue108AbortStreamInsteadOfAppendingProblem(t *testing.T) {
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("stream failure panic = %v, want http.ErrAbortHandler", got)
		}
	}()
	writeStreamAwareProblem(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/file", nil), true,
		http.StatusBadGateway, "Upstream transfer failed", "The Hub response could not be completely downloaded.")
}

func TestIssue108UnstartedResponseStillGetsProblem(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeStreamAwareProblem(recorder, httptest.NewRequest(http.MethodGet, "/file", nil), false,
		http.StatusBadGateway, "Upstream transfer failed", "The Hub response could not be completely downloaded.")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}
