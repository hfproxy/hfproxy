package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestIssue6RejectsMismatchedLFSContentBeforeStreamingOrCaching(t *testing.T) {
	advertised := []byte("verified model content for issue 6")
	corrupted := bytes.Clone(advertised)
	corrupted[0] ^= 0xff
	file := &auditHubFile{name: "model.bin", advertised: advertised, served: corrupted}
	envHolder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file.handler(envHolder.repoID).ServeHTTP(w, r)
	})
	env := newAuditStorageEnv(t, mux, auditStorageOptions{})
	*envHolder = *env

	response := env.get(context.Background(), file.name)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("mismatched LFS content status = %d, want 502; body=%q", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), corrupted) {
		t.Fatal("corrupted bytes were streamed before their advertised SHA-256 was verified")
	}
	if count := env.artifactCount(t, file.name); count != 0 {
		t.Fatalf("mismatched LFS content published %d artifact(s)", count)
	}
}
