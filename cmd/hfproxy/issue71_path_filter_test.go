package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/nicolas/hfproxy/internal/db"
)

func TestIssue71CacheInspectPathFilterTreatsUnderscoreLiterally(t *testing.T) {
	baseDSN := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open administrative connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	databaseName := "issue71_cli_" + strings.ReplaceAll(db.NewID(), "-", "")[:20]
	if _, err := admin.Exec(`CREATE DATABASE ` + databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + databaseName + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})
	databaseURL, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	databaseURL.Path = "/" + databaseName
	store, err := db.Open(context.Background(), databaseURL.String())
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(func() { _ = store.SQL.Close() })
	if err := db.Migrate(context.Background(), store.SQL); err != nil {
		t.Fatalf("migrate isolated database: %v", err)
	}
	repoID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO repositories(id,repo_type,repo_id) VALUES($1,'models','issue71/org')`, repoID); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	for i, file := range []string{"model_1.bin", "model-1.bin", "modelX1.bin"} {
		blobID := db.NewID()
		sha := fmt.Sprintf("%064x", i+1)
		if _, err := store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES($1,$2,1,$3)`, blobID, sha, "blobs/"+sha); err != nil {
			t.Fatalf("insert blob for %s: %v", file, err)
		}
		if _, err := store.SQL.Exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id) VALUES($1,$2,$3,$4,$5)`, db.NewID(), repoID, strings.Repeat("a", 40), file, blobID); err != nil {
			t.Fatalf("insert artifact %s: %v", file, err)
		}
	}

	t.Setenv("HFPROXY_DATABASE_URL", databaseURL.String())
	t.Setenv("HFPROXY_SESSION_SECRET", strings.Repeat("s", 40))
	t.Setenv("HFPROXY_S3_BUCKET", "issue71")
	t.Setenv("HFPROXY_S3_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStdout := os.Stdout
	os.Stdout = write
	runErr := run([]string{"cache", "inspect", "--path", "model_1.bin"})
	_ = write.Close()
	os.Stdout = previousStdout
	output, readErr := io.ReadAll(read)
	_ = read.Close()
	if readErr != nil {
		t.Fatalf("read CLI output: %v", readErr)
	}
	if runErr != nil {
		t.Fatalf("cache inspect failed: %v", runErr)
	}
	if got := string(output); !strings.Contains(got, "model_1.bin") || strings.Contains(got, "model-1.bin") || strings.Contains(got, "modelX1.bin") {
		t.Fatalf("cache inspect path filter returned unexpected rows: %q", got)
	}
}
