package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestMigrateSerializesConcurrentFirstRun(t *testing.T) {
	baseDSN := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open administrative connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	databaseName := "hfproxy_migrate_" + strings.ReplaceAll(NewID(), "-", "")[:20]
	if _, err := admin.Exec(`CREATE DATABASE ` + databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + databaseName + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})

	dsnURL, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	dsnURL.Path = "/" + databaseName
	const workers = 6
	databases := make([]*sql.DB, workers)
	for i := range databases {
		databases[i], err = sql.Open("pgx", dsnURL.String())
		if err != nil {
			t.Fatalf("open migration connection %d: %v", i, err)
		}
		defer databases[i].Close()
	}

	start := make(chan struct{})
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range databases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = Migrate(context.Background(), databases[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent migration %d: %v", i, err)
		}
	}

	var version int64
	if err := databases[0].QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(fmt.Errorf("read final migration version: %w", err))
	}
	if version != 4 {
		t.Fatalf("final migration version = %d, want 4", version)
	}
}
