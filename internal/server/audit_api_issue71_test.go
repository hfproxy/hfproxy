package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestIssue71PathFiltersTreatLikeMetacharactersLiterally(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	for _, file := range []string{"model_1.bin", "model-1.bin", "modelX1.bin"} {
		auditAPISeedArtifact(t, store, "models", "issue71/org", "public", file)
	}
	admin := auditAPIAdmin()
	admin.TokenID = "issue71-admin-token"
	admin.Scopes = []string{"admin", "write"}
	list := auditAPIDoAs(s, admin, http.MethodGet, "/api/v1/cache/entries?path=model_1.bin", "")
	var listed struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode cache entries response (status %d): %v; body=%s", list.Code, err, list.Body.String())
	}
	if list.Code != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Path != "model_1.bin" {
		t.Fatalf("path filter listed status=%d items=%+v, want only model_1.bin", list.Code, listed.Items)
	}

	purge := auditAPIDoAs(s, admin, http.MethodPost, "/api/v1/cache/purge?dry_run=true&path=model_1.bin", "")
	var impact struct {
		Entries int `json:"entries"`
	}
	if err := json.Unmarshal(purge.Body.Bytes(), &impact); err != nil {
		t.Fatalf("decode purge dry-run response (status %d): %v; body=%s", purge.Code, err, purge.Body.String())
	}
	if purge.Code != http.StatusOK || impact.Entries != 1 {
		t.Fatalf("purge filter status=%d entries=%d body=%s, want 200 and exactly 1", purge.Code, impact.Entries, purge.Body.String())
	}
}
