package server

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
)

func TestIssue32PurgeRacingRedownloadKeepsPublishedBlob(t *testing.T) {
	content := []byte("shared blob survives purge and re-download")
	file := &auditHubFile{name: "tokenizer.json", advertised: content, served: content}
	envHolder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file.handler(envHolder.repoID).ServeHTTP(w, r)
	})
	env := newAuditStorageEnv(t, mux, auditStorageOptions{mutate: func(cfg *config.Config) {
		cfg.CacheGCGrace = time.Second
	}})
	*envHolder = *env

	if response := env.get(context.Background(), file.name); response.Code != http.StatusOK {
		t.Fatalf("initial cache fill: status=%d body=%q", response.Code, response.Body.String())
	}
	var artifactID, blobID string
	if err := env.store.SQL.QueryRow(`SELECT a.id,b.id FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.path=$2`, env.repoPK, file.name).Scan(&artifactID, &blobID); err != nil {
		t.Fatalf("read initial cache entry: %v", err)
	}
	if err := env.server.purgeArtifact(context.Background(), artifactID); err != nil {
		t.Fatalf("purge artifact: %v", err)
	}
	if _, err := env.store.SQL.Exec(`UPDATE blobs SET orphaned_at=now()-interval '1 hour' WHERE id=$1`, blobID); err != nil {
		t.Fatalf("make orphan eligible for GC: %v", err)
	}

	start := make(chan struct{})
	gcDone := make(chan error, 1)
	getDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		gcDone <- env.server.collectOrphanedBlobs(context.Background())
	}()
	go func() {
		<-start
		getDone <- env.get(context.Background(), file.name)
	}()
	close(start)
	if err := <-gcDone; err != nil {
		t.Fatalf("collect orphan while re-download races: %v", err)
	}
	response := <-getDone
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), content) {
		t.Fatalf("re-download racing GC: status=%d body=%q", response.Code, response.Body.String())
	}

	var orphaned sql.NullTime
	var storageKey string
	if err := env.store.SQL.QueryRow(`SELECT b.orphaned_at,b.storage_key FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.path=$2`, env.repoPK, file.name).Scan(&orphaned, &storageKey); err != nil {
		t.Fatalf("read re-published cache entry: %v", err)
	}
	if orphaned.Valid {
		t.Fatal("re-published blob still has an orphan tombstone")
	}
	stored, err := env.objects.Get(context.Background(), storageKey, "")
	if err != nil {
		t.Fatalf("re-published S3 object is missing: %v", err)
	}
	defer stored.Body.Close()
	actual, err := io.ReadAll(stored.Body)
	if err != nil {
		t.Fatalf("read re-published S3 object: %v", err)
	}
	if !bytes.Equal(actual, content) {
		t.Fatalf("re-published S3 content = %q, want %q", actual, content)
	}
}
