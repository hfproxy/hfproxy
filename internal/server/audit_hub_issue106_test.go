package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssue106PreloadMissDoesNotStreamResponseBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/models/org/repo/resolve/main/weights.bin", nil)
	if !shouldStreamCacheMiss(request, "") {
		t.Fatal("ordinary full GET should retain streaming")
	}
	ctx := context.WithValue(request.Context(), preloadFetchContextKey{}, true)
	if shouldStreamCacheMiss(request.WithContext(ctx), "") {
		t.Fatal("preload GET must stage the body without streaming it to its recorder")
	}
	if shouldStreamCacheMiss(request.WithContext(ctx), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") {
		t.Fatal("preload GET with an LFS digest must not stream")
	}
}
