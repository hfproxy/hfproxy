package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
)

type flushProbe struct {
	bytes.Buffer
	flushes int
}

func (p *flushProbe) Flush() { p.flushes++ }

func TestStreamingWriterFlushesEachWrite(t *testing.T) {
	probe := &flushProbe{}
	writer := asStreamingWriter(probe)
	if _, err := writer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if probe.String() != "firstsecond" || probe.flushes != 2 {
		t.Fatalf("streaming writer output=%q flushes=%d", probe.String(), probe.flushes)
	}
}

func TestAuditOrphanedBlobGCDeletesAfterGrace(t *testing.T) {
	env := newAuditStorageEnv(t, http.NotFoundHandler(), auditStorageOptions{})
	data := []byte("orphaned cache object " + time.Now().UTC().Format(time.RFC3339Nano))
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	tmp, err := os.CreateTemp(t.TempDir(), "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := env.objects.UploadFile(context.Background(), tmp, env.objects.Key(hash), "application/octet-stream", int64(len(data))); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	id := db.NewID()
	_, err = env.store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key,orphaned_at) VALUES($1,$2,$3,$4,now()-interval '48 hours')`, id, hash, len(data), env.objects.Key(hash))
	if err != nil {
		t.Fatal(err)
	}
	if err := env.server.collectOrphanedBlobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := env.store.SQL.QueryRow(`SELECT COUNT(*) FROM blobs WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphaned blob row still exists: %d", count)
	}
	if _, err := env.objects.Head(context.Background(), env.objects.Key(hash)); err == nil {
		t.Fatal("orphaned object still exists in S3")
	}
}

func TestAuditIssue111GCCollectsWithZeroGrace(t *testing.T) {
	env := newAuditStorageEnv(t, http.NotFoundHandler(), auditStorageOptions{mutate: func(cfg *config.Config) {
		cfg.CacheGCGrace = 0
	}})
	ctx := context.Background()
	data := []byte("zero grace orphan " + time.Now().UTC().Format(time.RFC3339Nano))
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	tmp, err := os.CreateTemp(t.TempDir(), "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	key := env.objects.Key(hash)
	if err := env.objects.UploadFile(ctx, tmp, key, "application/octet-stream", int64(len(data))); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	id := db.NewID()
	if _, err := env.store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key,orphaned_at) VALUES($1,$2,$3,$4,now()-interval '1 second')`, id, hash, len(data), key); err != nil {
		t.Fatal(err)
	}
	if err := env.server.collectOrphanedBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := env.store.SQL.QueryRow(`SELECT COUNT(*) FROM blobs WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("zero grace left orphaned blob row: %d", count)
	}
	if _, err := env.objects.Head(ctx, key); err == nil {
		t.Fatal("zero grace left orphaned object in storage")
	}
}
