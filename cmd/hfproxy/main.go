package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/smithy-go"
	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/server"
	"github.com/nicolas/hfproxy/internal/storage"
)

var (
	version = "0.0.3"
	commit  = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hfproxy:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "--help", "-h", "help":
		printUsage()
		return nil
	case "version":
		fmt.Printf("hfproxy %s (commit %s)\n", version, commit)
		return nil
	case "config":
		if len(args) < 2 || args[1] != "validate" {
			return fmt.Errorf("usage: hfproxy config validate")
		}
		if _, err := config.Load(); err != nil {
			return err
		}
		fmt.Println("Configuration is valid.")
		return nil
	case "serve":
		return serve(args[1:])
	case "migrate":
		return migrate(args[1:])
	case "admin":
		return admin(args[1:])
	case "cache":
		return cache(args[1:])
	default:
		return fmt.Errorf("unknown command %q (try --help)", args[0])
	}
}

func printUsage() {
	fmt.Print(`hfproxy — internal Hugging Face Hub proxy

Usage:
  hfproxy serve
  hfproxy migrate up|status
  hfproxy admin create-user --username NAME --password-file PATH
  hfproxy admin reset-password --username NAME --password-file PATH
  hfproxy admin token create --username NAME [--name NAME] [--expires 720h]
  hfproxy admin token list|revoke --id ID
  hfproxy admin repo add --type models --id org/repo --policy public
  hfproxy admin identity link --provider oidc|ldap --username NAME --subject VALUE [--issuer URL]
  hfproxy cache inspect [--repo ID] [--path QUERY]
  hfproxy cache purge [--repo ID] [--path QUERY] [--execute --yes]
  hfproxy cache verify
  hfproxy config validate
  hfproxy version

Secrets are read from HFPROXY_*_FILE or protected mounted files; never pass them as flags.
`)
}

func logger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func serve(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hfproxy serve")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.SQL.Close()
	if cfg.AutoMigrate {
		if err := db.Migrate(ctx, store.SQL); err != nil {
			return err
		}
	}
	version, err := db.MigrationStatus(ctx, store.SQL)
	if err != nil {
		return err
	}
	if version < 4 {
		return fmt.Errorf("database is not migrated; run hfproxy migrate up")
	}
	cfg, err = config.LoadPersisted(ctx, store.SQL, cfg)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	objects, err := storage.New(ctx, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption)
	if err != nil {
		return err
	}
	app, err := server.New(ctx, cfg, store, objects, logger(cfg))
	if err != nil {
		return err
	}
	return app.ListenAndServe(ctx)
}

func migrate(args []string) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		return fmt.Errorf("usage: hfproxy migrate up|status")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.SQL.Close()
	if args[0] == "up" {
		if err := db.Migrate(ctx, store.SQL); err != nil {
			return err
		}
		fmt.Println("Database migrations applied.")
		return nil
	}
	version, err := db.MigrationStatus(ctx, store.SQL)
	if err != nil {
		return err
	}
	fmt.Printf("Schema version: %d\n", version)
	return nil
}

func admin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hfproxy admin create-user|reset-password|token|repo")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.SQL.Close()
	if args[0] == "create-user" {
		flags := flag.NewFlagSet("admin create-user", flag.ContinueOnError)
		username := flags.String("username", "", "local login name")
		passwordFile := flags.String("password-file", "", "path to a secret file mounted for this command")
		display := flags.String("display-name", "", "display name")
		email := flags.String("email", "", "email address")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *username == "" || *passwordFile == "" {
			return fmt.Errorf("--username and --password-file are required")
		}
		hash, err := passwordHashFromFile(*passwordFile)
		if err != nil {
			return err
		}
		if !validUsername(*username) {
			return fmt.Errorf("--username must be 1 to 64 letters, digits, dot, underscore, or hyphen")
		}
		var exists bool
		if err := store.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE role='admin')`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("an admin already exists")
		}
		id := db.NewID()
		_, err = store.SQL.ExecContext(ctx, `INSERT INTO users(id,username,email,display_name,password_hash,role) VALUES($1,$2,$3,$4,$5,'admin')`, id, *username, *email, *display, hash)
		if err != nil {
			return fmt.Errorf("create admin user: %w", err)
		}
		fmt.Printf("Created admin user %q (%s).\n", *username, id)
		return nil
	}
	if args[0] == "reset-password" {
		flags := flag.NewFlagSet("admin reset-password", flag.ContinueOnError)
		username := flags.String("username", "", "existing local admin login name")
		passwordFile := flags.String("password-file", "", "path to a secret file mounted for this command")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if !validUsername(*username) || *passwordFile == "" {
			return fmt.Errorf("a valid --username and --password-file are required")
		}
		hash, err := passwordHashFromFile(*passwordFile)
		if err != nil {
			return err
		}
		tx, err := store.SQL.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("start password reset: %w", err)
		}
		defer tx.Rollback()
		var userID string
		err = tx.QueryRowContext(ctx, `UPDATE users SET password_hash=$1,updated_at=now() WHERE username=$2 AND role='admin' RETURNING id`, hash, *username).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("admin user %q not found", *username)
		}
		if err != nil {
			return fmt.Errorf("reset admin password: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
			return fmt.Errorf("revoke admin sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,action,target_type,target_id) VALUES($1,'admin.password.reset','user',$2)`, db.NewID(), userID); err != nil {
			return fmt.Errorf("record password reset: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("save admin password: %w", err)
		}
		fmt.Printf("Reset password for admin user %q. Existing web sessions were revoked.\n", *username)
		return nil
	}
	if args[0] == "token" {
		if len(args) < 2 {
			return fmt.Errorf("usage: hfproxy admin token create|list|revoke")
		}
		return tokenCommand(ctx, store, cfg, args[1:])
	}
	if args[0] == "repo" {
		if len(args) < 2 || args[1] != "add" {
			return fmt.Errorf("usage: hfproxy admin repo add --type models --id org/repo --policy public")
		}
		flags := flag.NewFlagSet("admin repo add", flag.ContinueOnError)
		repoType := flags.String("type", "models", "models, datasets, or spaces")
		repoID := flags.String("id", "", "Hub repository ID")
		policy := flags.String("policy", "admin_only", "public, allowlist, or admin_only")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if !isRepoType(*repoType) || !validRepoID(*repoID) || (*policy != "public" && *policy != "allowlist" && *policy != "admin_only") {
			return fmt.Errorf("provide a valid --type, --id, and --policy")
		}
		id := db.NewID()
		version, err := db.MigrationStatus(ctx, store.SQL)
		if err != nil {
			return err
		}
		if version < 4 {
			return fmt.Errorf("database is not migrated; run hfproxy migrate up")
		}
		if _, err = store.SQL.ExecContext(ctx, `INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,$2,$3,$4) ON CONFLICT(repo_type,repo_id) DO UPDATE SET access_policy=EXCLUDED.access_policy,deleted_at=NULL,updated_at=now()`, id, *repoType, *repoID, *policy); err != nil {
			return err
		}
		fmt.Printf("Registered %s/%s with policy %s.\n", *repoType, *repoID, *policy)
		return nil
	}
	if args[0] == "identity" {
		if len(args) < 2 || args[1] != "link" {
			return fmt.Errorf("usage: hfproxy admin identity link --provider oidc|ldap --username NAME --subject VALUE [--issuer URL]")
		}
		flags := flag.NewFlagSet("admin identity link", flag.ContinueOnError)
		provider := flags.String("provider", "", "oidc or ldap")
		username := flags.String("username", "", "existing local account")
		issuer := flags.String("issuer", "", "identity issuer; defaults to configured provider")
		subject := flags.String("subject", "", "stable identity subject")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if *provider != "oidc" && *provider != "ldap" {
			return fmt.Errorf("--provider must be oidc or ldap")
		}
		if *username == "" || *subject == "" {
			return fmt.Errorf("--username and --subject are required")
		}
		if *issuer == "" {
			if *provider == "oidc" {
				*issuer = cfg.OIDCIssuer
			} else {
				*issuer = cfg.LDAPURL
			}
		}
		if *issuer == "" {
			return fmt.Errorf("the provider issuer is not configured; pass --issuer")
		}
		var userID string
		if err := store.SQL.QueryRowContext(ctx, "SELECT id FROM users WHERE username=$1", *username).Scan(&userID); err != nil {
			return fmt.Errorf("local user not found")
		}
		if _, err := store.SQL.ExecContext(ctx, "INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES($1,$2,$3,$4,$5)", db.NewID(), userID, *provider, *issuer, *subject); err != nil {
			return fmt.Errorf("link identity: %w", err)
		}
		_, _ = store.SQL.ExecContext(ctx, "INSERT INTO audit_events(id,actor_user_id,action,target_type,target_id,metadata_json) VALUES($1,$2,'identity.link','user',$2,'{}'::jsonb)", db.NewID(), userID)
		fmt.Printf("Linked %s identity to %s.\n", *provider, *username)
		return nil
	}
	return fmt.Errorf("unknown admin command %q", args[0])
}

func passwordHashFromFile(path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 2048))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	password := strings.TrimRight(string(data), "\r\n")
	hash, err := server.HashPassword(password)
	for i := range data {
		data[i] = 0
	}
	password = ""
	return hash, err
}

func tokenCommand(ctx context.Context, store *db.Store, cfg config.Config, args []string) error {
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("admin token create", flag.ContinueOnError)
		username := flags.String("username", "", "owner username")
		name := flags.String("name", "", "token label")
		expires := flags.Duration("expires", 0, "optional lifetime, such as 720h")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *username == "" {
			return fmt.Errorf("--username is required")
		}
		if *expires < 0 {
			return fmt.Errorf("--expires must be positive")
		}
		var userID string
		if err := store.SQL.QueryRowContext(ctx, `SELECT id FROM users WHERE username=$1 AND status='active'`, *username).Scan(&userID); err != nil {
			return fmt.Errorf("active user not found")
		}
		secret, err := randomToken()
		if err != nil {
			return err
		}
		digest := server.HashToken(secret, cfg.SessionSecret)
		prefix := secret[:16]
		id := db.NewID()
		var expiry any
		if *expires > 0 {
			expiry = time.Now().Add(*expires)
		}
		_, err = store.SQL.ExecContext(ctx, `INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, id, userID, digest, prefix, *name, expiry)
		if err != nil {
			return err
		}
		fmt.Printf("Token (shown once): %s\nID: %s\n", secret, id)
		return nil
	case "list":
		rows, err := store.SQL.QueryContext(ctx, `SELECT t.id,u.username,t.prefix,t.name,t.expires_at,t.revoked_at,t.last_used_at FROM access_tokens t JOIN users u ON u.id=t.user_id ORDER BY t.created_at DESC LIMIT 200`)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Printf("%-36s %-18s %-18s %-20s %-20s\n", "ID", "OWNER", "PREFIX", "EXPIRES", "LAST USED")
		for rows.Next() {
			var id, user, prefix, name string
			var expires, revoked, last sql.NullTime
			if err := rows.Scan(&id, &user, &prefix, &name, &expires, &revoked, &last); err != nil {
				return err
			}
			expireText := "never"
			if expires.Valid {
				expireText = expires.Time.Format(time.RFC3339)
			}
			lastText := "never"
			if last.Valid {
				lastText = last.Time.Format(time.RFC3339)
			}
			if revoked.Valid {
				expireText = "revoked"
			}
			fmt.Printf("%-36s %-18s %-18s %-20s %-20s\n", id, user, prefix, expireText, lastText)
		}
		return rows.Err()
	case "revoke":
		flags := flag.NewFlagSet("admin token revoke", flag.ContinueOnError)
		id := flags.String("id", "", "token ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		result, err := store.SQL.ExecContext(ctx, `UPDATE access_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, *id)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return fmt.Errorf("token not found")
		}
		fmt.Println("Token revoked.")
		return nil
	default:
		return fmt.Errorf("unknown token command %q", args[0])
	}
}

func cache(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hfproxy cache inspect|purge|verify")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.SQL.Close()
	if version, err := db.MigrationStatus(ctx, store.SQL); err != nil {
		return err
	} else if version < 4 {
		return fmt.Errorf("database is not migrated; run hfproxy migrate up")
	}
	cfg, err = config.LoadPersisted(ctx, store.SQL, cfg)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	objects, err := storage.New(ctx, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption)
	if err != nil {
		return err
	}
	switch args[0] {
	case "inspect":
		flags := flag.NewFlagSet("cache inspect", flag.ContinueOnError)
		repo := flags.String("repo", "", "repository filter")
		pathFilter := flags.String("path", "", "path substring")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		rows, err := store.SQL.QueryContext(ctx, `SELECT r.repo_type,r.repo_id,a.commit_sha,a.path,b.size_bytes,b.sha256,a.cached_at FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0) ORDER BY a.cached_at DESC LIMIT 200`, *repo, *pathFilter)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var typ, repoID, commit, file, sha string
			var size int64
			var cached time.Time
			if err := rows.Scan(&typ, &repoID, &commit, &file, &size, &sha, &cached); err != nil {
				return err
			}
			fmt.Printf("%s/%s@%s %s %d bytes sha256:%s %s\n", typ, repoID, commit, file, size, sha, cached.Format(time.RFC3339))
		}
		return rows.Err()
	case "purge":
		flags := flag.NewFlagSet("cache purge", flag.ContinueOnError)
		repo := flags.String("repo", "", "repository filter")
		pathFilter := flags.String("path", "", "path substring")
		execute := flags.Bool("execute", false, "perform deletion")
		yes := flags.Bool("yes", false, "confirm destructive purge")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		var count, bytes int64
		err = store.SQL.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0)`, *repo, *pathFilter).Scan(&count, &bytes)
		if err != nil {
			return err
		}
		fmt.Printf("Purge impact: %d entries, %s.\n", count, humanBytes(bytes))
		if !*execute {
			fmt.Println("Dry run only. Add --execute --yes to purge.")
			return nil
		}
		if !*yes {
			return fmt.Errorf("purge requires both --execute and --yes")
		}
		rows, err := store.SQL.QueryContext(ctx, `SELECT a.id FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0)`, *repo, *pathFilter)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if err := purgeCLI(ctx, store, objects, id); err != nil {
				return err
			}
		}
		fmt.Printf("Purged %d entries.\n", len(ids))
		return nil
	case "verify":
		checked, invalid := 0, 0
		var cursorTime time.Time
		var cursorID string
		for {
			query := `SELECT id,sha256,storage_key,size_bytes,created_at FROM blobs ORDER BY created_at,id LIMIT 200`
			args := []any{}
			if cursorID != "" {
				query = `SELECT id,sha256,storage_key,size_bytes,created_at FROM blobs WHERE (created_at,id) > ($1,$2) ORDER BY created_at,id LIMIT 200`
				args = []any{cursorTime, cursorID}
			}
			rows, err := store.SQL.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			pageCount := 0
			for rows.Next() {
				var id, sha, key string
				var expectedSize int64
				var createdAt time.Time
				if err := rows.Scan(&id, &sha, &key, &expectedSize, &createdAt); err != nil {
					rows.Close()
					return err
				}
				cursorTime, cursorID = createdAt, id
				pageCount++
				objectCtx, objectCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				out, err := objects.Get(objectCtx, key, "")
				if err != nil {
					objectCancel()
					var apiErr smithy.APIError
					if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
						fmt.Printf("MISSING %s %s\n", id, sha)
					} else {
						fmt.Printf("ERROR %s %s: %v\n", id, sha, err)
					}
					invalid++
					checked++
					continue
				}
				digest := sha256.New()
				size, copyErr := io.Copy(digest, out.Body)
				_ = out.Body.Close()
				objectCancel()
				actual := hex.EncodeToString(digest.Sum(nil))
				if copyErr != nil || size != expectedSize || actual != sha {
					fmt.Printf("INVALID %s expected=%s actual=%s bytes=%d/%d\n", id, sha, actual, size, expectedSize)
					invalid++
				}
				checked++
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			if pageCount < 200 {
				break
			}
		}
		fmt.Printf("Checked %d blobs; %d invalid or missing.\n", checked, invalid)
		if invalid > 0 {
			return errors.New("one or more cache objects failed verification")
		}
		return nil
	default:
		return fmt.Errorf("unknown cache command %q", args[0])
	}
}

func purgeCLI(ctx context.Context, store *db.Store, objects *storage.S3, id string) error {
	tx, err := store.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var blobID string
	if err := tx.QueryRowContext(ctx, `SELECT b.id FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.id=$1 FOR UPDATE OF a`, id).Scan(&blobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1`, blobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(id,action,target_type,target_id,metadata_json) VALUES($1,'cache.entry.purge','artifact',$2,'{\"source\":\"cli\"}'::jsonb)", db.NewID(), id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = objects // retained for the CLI's storage lifecycle and future GC hooks
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "hfpx_" + base64.RawURLEncoding.EncodeToString(b), nil
}
func isRepoType(value string) bool {
	return value == "models" || value == "datasets" || value == "spaces"
}
func validRepoID(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.", r)) {
				return false
			}
		}
	}
	return true
}
func validUsername(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}
func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= 1024
		if v < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
	}
	return "0 B"
}
