package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestIssue90OperatorPurgeRespectsRepositoryVisibility(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	_, token := auditAPICreateUser(t, store, "operator", "operator")
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET scopes=ARRAY['write'] WHERE token_hash=$1`, HashToken(token, auditAPISessionSecret)); err != nil {
		t.Fatal(err)
	}
	repoID := auditAPISeedArtifact(t, store, "models", "org/hidden", "admin_only", "weights.safetensors")
	var artifactID string
	if err := store.SQL.QueryRow(`SELECT id FROM artifacts WHERE repository_id=$1 AND path='weights.safetensors'`, repoID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}

	dry := auditAPIDo(s, http.MethodPost, "/api/v1/cache/purge?repo_id=org%2Fhidden", token, "")
	if dry.Code != http.StatusOK || strings.Contains(dry.Body.String(), `"entries":1`) {
		t.Fatalf("hidden repo purge dry-run = %d %s", dry.Code, dry.Body.String())
	}
	entry := auditAPIDo(s, http.MethodPost, "/api/v1/cache/entries/"+artifactID+"/purge", token, "")
	if entry.Code != http.StatusNotFound {
		t.Fatalf("hidden cache entry purge status = %d, want 404: %s", entry.Code, entry.Body.String())
	}
	var count int
	if err := store.SQL.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE id=$1`, artifactID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("operator purge removed an invisible cache entry")
	}
}
