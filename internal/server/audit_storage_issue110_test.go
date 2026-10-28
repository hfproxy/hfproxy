package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestIssue110RefillRestoresMissingBlobObject(t *testing.T) {
	content := []byte("issue-110-" + auditRandomHex(300))
	file := &auditHubFile{name: "refill.txt", advertised: content, served: content}
	holder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { file.handler(holder.repoID).ServeHTTP(w, r) })
	env := newAuditStorageEnv(t, mux, auditStorageOptions{})
	*holder = *env
	ctx := context.Background()
	if rec := env.get(ctx, file.name); rec.Code != 200 {
		t.Fatalf("initial miss: %d", rec.Code)
	}
	var artifactID, key string
	if err := env.store.SQL.QueryRow(`SELECT a.id,b.storage_key FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.path=$2`, env.repoPK, file.name).Scan(&artifactID, &key); err != nil {
		t.Fatal(err)
	}
	if err := env.server.purgeArtifact(ctx, artifactID); err != nil {
		t.Fatal(err)
	}
	// Recreate the state left if object deletion succeeds but deleting the blob
	// row fails before the garbage collection transaction commits.
	if err := env.objects.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	rec := env.get(ctx, file.name)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("refill returned status %d and body length %d; want 200 and %d bytes", rec.Code, rec.Body.Len(), len(content))
	}
}
