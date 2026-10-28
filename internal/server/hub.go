package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
)

type artifact struct {
	ID          string
	Repository  string
	Commit      string
	Path        string
	BlobID      string
	StorageKey  string
	SHA256      string
	Size        int64
	ETag        string
	ContentType string
	LastMod     sql.NullTime
}

func shouldStreamCacheMiss(r *http.Request, expectedLFS string) bool {
	return expectedLFS == "" && !isPreloadFetch(r.Context()) && r.Method == http.MethodGet &&
		r.Header.Get("Range") == "" && r.Header.Get("If-Range") == "" &&
		r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") == ""
}

type resolvePath struct{ RepoType, RepoID, Revision, FilePath string }

type downloadProgressWriter struct {
	ctx         context.Context
	db          *sql.DB
	id          string
	baseBytes   int64
	destination io.Writer
	bytes       int64
	lastPersist time.Time
}

func (w *downloadProgressWriter) Write(chunk []byte) (int, error) {
	n, err := w.destination.Write(chunk)
	w.bytes += int64(n)
	if time.Since(w.lastPersist) >= time.Second {
		w.lastPersist = time.Now()
		_, _ = w.db.ExecContext(w.ctx, `UPDATE downloads SET bytes=$2 WHERE id=$1 AND status='downloading'`, w.id, w.baseBytes+w.bytes)
	}
	return n, err
}

func upstreamResolvePath(parsed resolvePath) string {
	repoTypePrefix := ""
	if parsed.RepoType != "models" {
		repoTypePrefix = parsed.RepoType + "/"
	}
	return "/" + repoTypePrefix + escapeRepoPath(parsed.RepoID) + "/resolve/" + url.PathEscape(parsed.Revision) + "/" + escapeFilePath(parsed.FilePath)
}

func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	parsed, err := parseResolvePath(r.URL.EscapedPath())
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	p, authErr := s.authenticate(r)
	repoPK, policy, err := s.repositoryAccess(r.Context(), p, authErr, parsed.RepoType, parsed.RepoID)
	if err != nil {
		s.accessProblem(w, r, err)
		return
	}
	resolvedCommit := ""
	if validCommit(parsed.Revision) {
		resolvedCommit = parsed.Revision
	} else {
		_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2 AND resolved_at>now()-($3 * interval '1 second')`, repoPK, parsed.Revision, s.currentConfig().CacheRefTTL.Seconds()).Scan(&resolvedCommit)
		if !validCommit(resolvedCommit) {
			resolvedCommit = ""
		}
	}
	if resolvedCommit != "" {
		if cached, lookupErr := s.findArtifact(r.Context(), s.db.SQL, repoPK, resolvedCommit, parsed.FilePath); lookupErr == nil {
			_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1`, cached.ID)
			s.recordDownload(r.Context(), repoPK, resolvedCommit, parsed.FilePath, "hit", cached.Size, "")
			s.serveOrAcknowledgePreload(w, r, cached, "HIT")
			return
		} else if !isNoRows(lookupErr) {
			writeProblem(w, r, 500, "Cache metadata error", "Unable to read cache metadata.")
			return
		}
	}
	requestURL := upstreamResolvePath(parsed)
	negativeKey := repoPK + "|" + parsed.Revision + "|" + parsed.FilePath
	if status, headers, ok := s.negativeLookup(negativeKey); ok {
		s.hubStatus(w, r, status, headers)
		return
	}
	upstream, firstHeaders, err := s.fetchHub(r.Context(), http.MethodHead, requestURL, safeResolveQuery(r.URL.Query()), nil)
	if err != nil {
		s.hubFailure(w, r, err)
		return
	}
	defer upstream.Body.Close()
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if upstream.StatusCode == http.StatusNotFound || upstream.StatusCode == http.StatusGone {
			s.negativeStore(negativeKey, upstream.StatusCode, upstream.Header)
		}
		s.hubStatus(w, r, upstream.StatusCode, upstream.Header)
		return
	}
	commit := firstHeaders.Get("X-Repo-Commit")
	if commit == "" {
		commit = upstream.Header.Get("X-Repo-Commit")
	}
	if commit == "" && validCommit(parsed.Revision) {
		commit = parsed.Revision
	}
	if commit == "" || !validCommit(commit) {
		commit, err = s.resolveCommit(r.Context(), parsed)
		if err != nil {
			s.hubFailure(w, r, err)
			return
		}
	}
	if !validCommit(parsed.Revision) {
		_, _ = s.db.SQL.ExecContext(r.Context(), `INSERT INTO revisions(id,repository_id,requested_ref,commit_sha,resolved_at) VALUES($1,$2,$3,$4,now())
			ON CONFLICT(repository_id,requested_ref) DO UPDATE SET commit_sha=EXCLUDED.commit_sha,resolved_at=now()`, db.NewID(), repoPK, parsed.Revision, commit)
	}
	meta := artifact{Repository: repoPK, Commit: commit, Path: parsed.FilePath, ETag: upstream.Header.Get("ETag"), ContentType: upstream.Header.Get("Content-Type")}
	if linked := firstHeaders.Get("X-Linked-ETag"); lfsSHA256(linked) != "" {
		meta.ETag = linked
	} else if meta.ETag == "" {
		meta.ETag = linked
	}
	if lm := upstream.Header.Get("Last-Modified"); lm != "" {
		if t, e := http.ParseTime(lm); e == nil {
			meta.LastMod = sql.NullTime{Time: t, Valid: true}
		}
	}
	sizeKnown := false
	if size := firstHeaders.Get("X-Linked-Size"); size != "" {
		if parsedSize, parseErr := strconv.ParseInt(size, 10, 64); parseErr == nil && parsedSize >= 0 {
			meta.Size = parsedSize
			sizeKnown = true
		}
	}
	if !sizeKnown && upstream.ContentLength >= 0 {
		meta.Size = upstream.ContentLength
		sizeKnown = true
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	cached, cacheErr := s.findArtifact(r.Context(), s.db.SQL, repoPK, commit, parsed.FilePath)
	if cacheErr != nil && !isNoRows(cacheErr) {
		writeProblem(w, r, 500, "Cache metadata error", "Unable to read cache metadata.")
		return
	}
	if cacheErr == nil {
		_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1`, cached.ID)
		s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
		s.serveOrAcknowledgePreload(w, r, cached, "HIT")
		return
	}
	if r.Method == http.MethodHead {
		if ifNoneMatch(r.Header.Get("If-None-Match"), meta.ETag) || (r.Header.Get("If-None-Match") == "" && meta.LastMod.Valid && notModifiedSince(r.Header.Get("If-Modified-Since"), meta.LastMod.Time)) {
			s.setArtifactHeaders(w, meta, upstream.Header)
			w.Header().Set("X-Repo-Commit", commit)
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s.setArtifactHeaders(w, meta, upstream.Header)
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Repo-Commit", commit)
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	if policy == "admin_only" && p == nil {
		writeProblem(w, r, 401, "Authentication required", "Sign in to access this repository.")
		return
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to coordinate this download.")
		return
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if cached, err = s.findArtifact(r.Context(), tx, repoPK, commit, parsed.FilePath); err == nil {
		if err := tx.Commit(); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to finish cache lookup.")
			return
		}
		s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
		s.serveOrAcknowledgePreload(w, r, cached, "HIT")
		return
	} else if !isNoRows(err) {
		writeProblem(w, r, 500, "Cache metadata error", "Unable to read cache metadata.")
		return
	}
	// Do not hold a database connection while downloading from the Hub or
	// uploading to object storage; progress writes use the pool separately.
	_ = tx.Rollback()
	tx = nil
	missKey := repoPK + "|" + commit + "|" + parsed.FilePath
	leader, done := s.joinCacheMiss(missKey)
	if !leader {
		select {
		case <-done:
			s.resolveFile(w, r)
		case <-r.Context().Done():
		}
		return
	}
	defer s.finishCacheMiss(missKey)
	downloadID := db.NewID()
	_, _ = s.db.SQL.ExecContext(r.Context(), `INSERT INTO downloads(id,repository_id,commit_sha,path,status,bytes) VALUES($1,$2,$3,$4,'downloading',0)`, downloadID, repoPK, commit, parsed.FilePath)
	objectStore := s.currentStorage()
	if meta.Size > s.currentConfig().CacheMaxFileSize {
		s.finishDownload(r.Context(), downloadID, 0, "size_limit")
		writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
		return
	}
	var tmp *os.File
	var body *http.Response
	var size int64
	var digest string
	var resume *downloadResume
	streamed := false
	expectedLFS := lfsSHA256(firstHeaders.Get("X-Linked-ETag"))
	if sizeKnown {
		resumeETag := upstream.Header.Get("ETag")
		if resumeETag == "" {
			resumeETag = firstHeaders.Get("ETag")
		}
		var stream io.Writer
		if shouldStreamCacheMiss(r, expectedLFS) {
			s.setArtifactHeaders(w, meta, firstHeaders)
			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("X-Repo-Commit", commit)
			w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
			w.WriteHeader(http.StatusOK)
			stream = asStreamingWriter(w)
			streamed = true
		}
		tmp, digest, resume, err = s.downloadResumable(r.Context(), objectStore, downloadID, repoPK, commit, parsed, meta.Size, resumeETag, meta.ContentType, stream)
		size = meta.Size
		if err != nil {
			completed := int64(0)
			if resume != nil {
				completed = resume.CompletedBytes
			}
			s.finishDownload(r.Context(), downloadID, completed, "upstream_read")
			var statusErr *upstreamStatusError
			if errors.As(err, &statusErr) {
				s.hubStatus(w, r, statusErr.status, statusErr.header)
				return
			}
			writeProblem(w, r, http.StatusBadGateway, "Upstream transfer failed", "The file could not be completed. Retry the preload to continue from the last stored block.")
			return
		}
		defer func() {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}()
	} else {
		fetchPath := upstreamResolvePath(resolvePath{RepoType: parsed.RepoType, RepoID: parsed.RepoID, Revision: commit, FilePath: parsed.FilePath})
		var headers http.Header
		body, headers, err = s.fetchHub(r.Context(), http.MethodGet, fetchPath, safeResolveQuery(r.URL.Query()), nil)
		if err != nil {
			s.finishDownload(r.Context(), downloadID, 0, "upstream")
			s.hubFailure(w, r, err)
			return
		}
		defer body.Body.Close()
		if body.StatusCode < 200 || body.StatusCode >= 300 {
			s.finishDownload(r.Context(), downloadID, 0, "upstream_status")
			s.hubStatus(w, r, body.StatusCode, body.Header)
			return
		}
		actualCommit := headers.Get("X-Repo-Commit")
		if actualCommit == "" {
			actualCommit = body.Header.Get("X-Repo-Commit")
		}
		if actualCommit != "" && actualCommit != commit {
			s.finishDownload(r.Context(), downloadID, 0, "revision_changed")
			writeProblem(w, r, http.StatusConflict, "Revision changed", "The requested branch moved during the download; retry against the resolved revision.")
			return
		}
		if body.ContentLength > s.currentConfig().CacheMaxFileSize {
			s.finishDownload(r.Context(), downloadID, 0, "size_limit")
			writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
			return
		}
		tmp, err = os.CreateTemp("", "hfproxy-staging-*")
		if err != nil {
			s.finishDownload(r.Context(), downloadID, 0, "staging")
			writeProblem(w, r, 500, "Cache error", "Unable to stage the upstream file.")
			return
		}
		defer func() {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}()
		hash := sha256.New()
		limited := io.LimitReader(body.Body, s.currentConfig().CacheMaxFileSize+1)
		destination := io.Writer(io.MultiWriter(tmp, hash))
		if shouldStreamCacheMiss(r, expectedLFS) {
			s.setArtifactHeaders(w, meta, headers)
			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("X-Repo-Commit", commit)
			if body.ContentLength >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(body.ContentLength, 10))
			}
			w.WriteHeader(http.StatusOK)
			destination = io.MultiWriter(tmp, hash, asStreamingWriter(w))
			streamed = true
		}
		progress := &downloadProgressWriter{ctx: r.Context(), db: s.db.SQL, id: downloadID, destination: destination, lastPersist: time.Now()}
		size, err = io.Copy(progress, limited)
		if err != nil {
			s.finishDownload(r.Context(), downloadID, size, "upstream_read")
			writeProblem(w, r, 502, "Upstream transfer failed", "The Hub response could not be completely downloaded.")
			return
		}
		if size > s.currentConfig().CacheMaxFileSize {
			s.finishDownload(r.Context(), downloadID, size, "size_limit")
			writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
			return
		}
		digest = hex.EncodeToString(hash.Sum(nil))
	}
	if expectedLFS != "" && digest != expectedLFS {
		if resume != nil {
			_ = s.resetDownloadResume(r.Context(), objectStore, resume, resume.ETag, resume.TotalSize, resume.ContentType)
		}
		s.finishDownload(r.Context(), downloadID, size, "integrity")
		writeProblem(w, r, http.StatusBadGateway, "Integrity check failed", "The downloaded file did not match the Hub's advertised SHA-256.")
		return
	}
	key := objectStore.Key(digest)
	contentType := meta.ContentType
	if body != nil && body.Header.Get("Content-Type") != "" {
		contentType = body.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(parsed.FilePath))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if resume == nil {
		if err := tmp.Sync(); err != nil {
			s.finishDownload(r.Context(), downloadID, size, "staging")
			writeProblem(w, r, 500, "Cache error", "Unable to finalize the staged file.")
			return
		}
	}
	tx, err = s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		s.finishDownload(r.Context(), downloadID, size, "metadata")
		writeProblem(w, r, 500, "Cache metadata error", "Unable to open cache metadata transaction.")
		return
	}
	var blobID, existingKey string
	err = tx.QueryRowContext(r.Context(), `SELECT id,storage_key FROM blobs WHERE sha256=$1 FOR UPDATE`, digest).Scan(&blobID, &existingKey)
	if err == sql.ErrNoRows {
		if err = objectStore.UploadFile(r.Context(), tmp, key, contentType, size); err != nil {
			s.finishDownload(r.Context(), downloadID, size, "storage")
			writeProblem(w, r, 502, "Storage error", "The downloaded file could not be published to object storage.")
			return
		}
		err = tx.QueryRowContext(r.Context(), `INSERT INTO blobs(id,sha256,size_bytes,storage_key,content_type,verified_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(sha256) DO UPDATE SET orphaned_at=NULL RETURNING id`, db.NewID(), digest, size, key, contentType).Scan(&blobID)
		if err != nil {
			s.finishDownload(r.Context(), downloadID, size, "metadata")
			writeProblem(w, r, 500, "Cache metadata error", "Unable to publish cache metadata.")
			return
		}
	} else if err != nil {
		s.finishDownload(r.Context(), downloadID, size, "metadata")
		writeProblem(w, r, 500, "Cache metadata error", "Unable to read blob metadata.")
		return
	} else {
		key = existingKey
		if _, err = tx.ExecContext(r.Context(), `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, blobID); err != nil {
			s.finishDownload(r.Context(), downloadID, size, "metadata")
			writeProblem(w, r, 500, "Cache metadata error", "Unable to restore cache metadata.")
			return
		}
	}
	meta.Size = size
	meta.ContentType = contentType
	if meta.ETag == "" && body != nil {
		meta.ETag = body.Header.Get("ETag")
	}
	if body != nil {
		if lm := body.Header.Get("Last-Modified"); lm != "" {
			if t, e := http.ParseTime(lm); e == nil {
				meta.LastMod = sql.NullTime{Time: t, Valid: true}
			}
		}
	}
	artifactID := db.NewID()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,etag,last_modified,upstream_size) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(repository_id,commit_sha,path) DO UPDATE SET blob_id=EXCLUDED.blob_id,etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,upstream_size=EXCLUDED.upstream_size,cached_at=now(),last_accessed_at=now()`, artifactID, repoPK, commit, parsed.FilePath, blobID, meta.ETag, nullTime(meta.LastMod), size); err != nil {
		s.finishDownload(r.Context(), downloadID, size, "metadata")
		writeProblem(w, r, 500, "Cache metadata error", "Unable to publish artifact metadata.")
		return
	}
	if err = tx.Commit(); err != nil {
		s.finishDownload(r.Context(), downloadID, size, "metadata")
		writeProblem(w, r, 500, "Cache metadata error", "Unable to commit artifact metadata.")
		return
	}
	if resume != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.finishDownloadResume(cleanupCtx, objectStore, resume)
		cancel()
	}
	s.finishDownload(r.Context(), downloadID, size, "")
	if streamed {
		return
	}
	cached, err = s.findArtifact(r.Context(), s.db.SQL, repoPK, commit, parsed.FilePath)
	if err != nil {
		writeProblem(w, r, 500, "Cache metadata error", "The artifact was stored but could not be read back.")
		return
	}
	s.serveOrAcknowledgePreload(w, r, cached, "MISS")
}

func (s *Server) legacyNegativeLookup(key string) (int, http.Header, bool) {
	s.negativeMu.Lock()
	defer s.negativeMu.Unlock()
	entry, ok := s.negative[key]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(s.negative, key)
		}
		return 0, nil, false
	}
	return entry.status, entry.headers.Clone(), true
}

func (s *Server) legacyNegativeStore(key string, status int, headers http.Header) {
	s.negativeMu.Lock()
	defer s.negativeMu.Unlock()
	if s.negative == nil {
		s.negative = make(map[string]negativeCacheEntry)
	}
	selected := make(http.Header)
	for _, name := range []string{"X-Error-Code", "X-Error-Message", "X-Repo-Commit", "Retry-After"} {
		if value := headers.Get(name); value != "" {
			selected.Set(name, value)
		}
	}
	s.negative[key] = negativeCacheEntry{expires: time.Now().Add(s.currentConfig().CacheNegativeTTL), status: status, headers: selected}
}

func (s *Server) legacyJoinCacheMiss(key string) (bool, <-chan struct{}) {
	s.missMu.Lock()
	defer s.missMu.Unlock()
	if s.misses == nil {
		s.misses = make(map[string]*cacheMiss)
	}
	if flight, ok := s.misses[key]; ok {
		return false, flight.done
	}
	s.misses[key] = &cacheMiss{done: make(chan struct{})}
	return true, s.misses[key].done
}

func (s *Server) legacyFinishCacheMiss(key string) {
	s.missMu.Lock()
	defer s.missMu.Unlock()
	if flight, ok := s.misses[key]; ok {
		delete(s.misses, key)
		close(flight.done)
	}
}

func (s *Server) legacyStartOrphanedBlobGC(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = s.collectOrphanedBlobs(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Server) legacyCollectOrphanedBlobs(ctx context.Context) error {
	objects := s.currentStorage()
	if objects == nil || s.currentConfig().CacheGCGrace <= 0 {
		return nil
	}
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id FROM blobs b
		WHERE b.orphaned_at IS NOT NULL AND b.orphaned_at < now()-$1::interval
		AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id)`, s.currentConfig().CacheGCGrace.String())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		tx, err := s.db.SQL.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var key string
		if err = tx.QueryRowContext(ctx, `SELECT storage_key FROM blobs WHERE id=$1 AND orphaned_at IS NOT NULL AND orphaned_at < now()-$2::interval FOR UPDATE`, id, s.currentConfig().CacheGCGrace.String()).Scan(&key); err == sql.ErrNoRows {
			_ = tx.Rollback()
			continue
		} else if err != nil {
			_ = tx.Rollback()
			return err
		}
		var refs int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, id).Scan(&refs); err != nil {
			_ = tx.Rollback()
			return err
		}
		if refs > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, id)
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if err != nil {
				return err
			}
			continue
		}
		// Hold the blob row lock while deleting the object. A concurrent cache
		// fill must lock this row before reusing the same digest.
		if err = objects.Delete(ctx, key); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM blobs WHERE id=$1`, id); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func parseResolvePath(escaped string) (resolvePath, error) {
	if len(escaped) > 8192 {
		return resolvePath{}, fmt.Errorf("request path exceeds the configured limit")
	}
	parts := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	repoStart := 0
	repoType := "models"
	if len(parts) > 0 && validRepoType(parts[0]) {
		repoType = parts[0]
		repoStart = 1
	}
	if len(parts) < repoStart+4 {
		return resolvePath{}, fmt.Errorf("expected /{repo}/resolve/{revision}/{file} or /{models|datasets|spaces}/{repo}/resolve/{revision}/{file}")
	}
	marker := -1
	for i := repoStart + 1; i+2 < len(parts); i++ {
		if parts[i] == "resolve" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return resolvePath{}, fmt.Errorf("resolve route is incomplete")
	}
	repoParts := make([]string, marker-repoStart)
	for i := repoStart; i < marker; i++ {
		if strings.Contains(strings.ToLower(parts[i]), "%2f") || strings.Contains(strings.ToLower(parts[i]), "%5c") {
			return resolvePath{}, fmt.Errorf("encoded path separators are not allowed in repository identifiers")
		}
		decoded, err := url.PathUnescape(parts[i])
		if err != nil {
			return resolvePath{}, fmt.Errorf("invalid URL encoding")
		}
		repoParts[i-repoStart] = decoded
	}
	repoID := strings.Join(repoParts, "/")
	if !validRepoID(repoID) {
		return resolvePath{}, fmt.Errorf("repository identifier is invalid")
	}
	revisionRaw := parts[marker+1]
	revision, err := url.PathUnescape(revisionRaw)
	if err != nil || !validRevision(revision) {
		return resolvePath{}, fmt.Errorf("revision is invalid")
	}
	fileParts := make([]string, 0, len(parts)-marker-2)
	for _, raw := range parts[marker+2:] {
		if strings.Contains(strings.ToLower(raw), "%2f") || strings.Contains(strings.ToLower(raw), "%5c") {
			return resolvePath{}, fmt.Errorf("encoded separators are not allowed in file paths")
		}
		part, err := url.PathUnescape(raw)
		if err != nil {
			return resolvePath{}, fmt.Errorf("invalid URL encoding")
		}
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") || hasControl(part) {
			return resolvePath{}, fmt.Errorf("file path contains an invalid segment")
		}
		fileParts = append(fileParts, part)
	}
	filePath := strings.Join(fileParts, "/")
	if filePath == "" || len(filePath) > 4096 {
		return resolvePath{}, fmt.Errorf("file path is empty or too long")
	}
	return resolvePath{RepoType: repoType, RepoID: repoID, Revision: revision, FilePath: filePath}, nil
}

func validRevision(revision string) bool {
	if revision == "" || len(revision) > 512 || strings.HasPrefix(revision, "/") || strings.HasSuffix(revision, "/") || strings.ContainsAny(revision, "\\\x00") || hasControl(revision) {
		return false
	}
	for _, p := range strings.Split(revision, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}
func validCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func lfsSHA256(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "W/")
	value = strings.Trim(value, `"`)
	if len(value) != sha256.Size*2 {
		return ""
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return strings.ToLower(value)
}
func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
func escapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func escapeFilePath(file string) string {
	parts := strings.Split(file, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

type accessError struct {
	status        int
	title, detail string
}

func (e accessError) Error() string { return e.detail }
func (s *Server) repositoryAccess(ctx context.Context, p *Principal, authErr error, repoType, repoID string) (string, string, error) {
	var id, policy string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT id,access_policy FROM repositories WHERE repo_type=$1 AND repo_id=$2 AND deleted_at IS NULL`, repoType, repoID).Scan(&id, &policy)
	if err == sql.ErrNoRows {
		return "", "", accessError{http.StatusForbidden, "Repository not registered", "This repository has no local access policy."}
	}
	if err != nil {
		return "", "", err
	}
	if p == nil {
		if authErr != errNoCredentials || !s.currentConfig().AnonymousPublicRead || policy != "public" {
			return "", "", accessError{http.StatusUnauthorized, "Authentication required", "Sign in to access this repository."}
		}
		return id, policy, nil
	}
	if p.Role == "admin" {
		return id, policy, nil
	}
	switch policy {
	case "public":
		return id, policy, nil
	case "admin_only":
		return "", "", accessError{http.StatusForbidden, "Forbidden", "This repository is restricted to administrators."}
	case "allowlist":
		var allowed bool
		err := s.db.SQL.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM repo_grants g WHERE g.repository_id=$1 AND g.permission IN ('read','write','admin') AND (
			(g.subject_type='user' AND g.subject_id=$2) OR (g.subject_type='group' AND EXISTS(SELECT 1 FROM group_members m WHERE m.group_id=g.subject_id AND m.user_id=$2))))`, id, p.ID).Scan(&allowed)
		if err != nil {
			return "", "", err
		}
		if !allowed {
			return "", "", accessError{http.StatusForbidden, "Forbidden", "You do not have read access to this repository."}
		}
		return id, policy, nil
	}
	return "", "", accessError{http.StatusForbidden, "Forbidden", "Repository access denied."}
}

func (s *Server) accessProblem(w http.ResponseWriter, r *http.Request, err error) {
	if a, ok := err.(accessError); ok {
		if a.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		}
		writeProblem(w, r, a.status, a.title, a.detail)
		return
	}
	writeProblem(w, r, 500, "Authorization error", "Unable to evaluate repository access.")
}

type fetchResult struct {
	*http.Response
	First http.Header
}

type idleBody struct {
	body    io.ReadCloser
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	closed  bool
}

type flushWriter struct {
	io.Writer
	flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil {
		w.flusher.Flush()
	}
	return n, err
}

func asStreamingWriter(w io.Writer) io.Writer {
	if flusher, ok := w.(http.Flusher); ok {
		return flushWriter{Writer: w, flusher: flusher}
	}
	return w
}

func newIdleBody(body io.ReadCloser, timeout time.Duration) *idleBody {
	return &idleBody{body: body, timeout: timeout}
}

func (b *idleBody) resetTimer() {
	if b.timeout <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, func() {
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				return
			}
			b.closed = true
			b.mu.Unlock()
			_ = b.body.Close()
		})
	} else {
		b.timer.Reset(b.timeout)
	}
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.resetTimer()
	n, err := b.body.Read(p)
	b.resetTimer()
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.body.Close()
}

func (s *Server) fetchHub(ctx context.Context, method, requestPath string, query url.Values, headers http.Header) (*http.Response, http.Header, error) {
	target := strings.TrimRight(s.currentConfig().HFBaseURL, "/") + requestPath
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	baseURL, _ := url.Parse(s.currentConfig().HFBaseURL)
	baseHost := strings.ToLower(baseURL.Hostname())
	var first http.Header
	for hops := 0; hops <= 5; hops++ {
		u, err := url.Parse(target)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid upstream URL")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && configLoopback(u.Hostname())) {
			return nil, nil, fmt.Errorf("unsafe upstream redirect")
		}
		if u.User != nil || u.Host == "" {
			return nil, nil, fmt.Errorf("unsafe upstream URL")
		}
		if port := u.Port(); port != "" && port != "443" && !(port == "80" && configLoopback(u.Hostname())) {
			return nil, nil, fmt.Errorf("unsafe upstream port")
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			return nil, nil, fmt.Errorf("build upstream request")
		}
		req.Header.Set("User-Agent", "hfproxy/"+Version)
		req.Header.Set("Accept-Encoding", "identity")
		for key, values := range headers {
			if isHopByHop(key) || strings.HasPrefix(strings.ToLower(key), "proxy-") {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		if s.currentConfig().HFToken != "" && strings.EqualFold(u.Hostname(), baseHost) {
			req.Header.Set("Authorization", "Bearer "+s.currentConfig().HFToken)
		}
		resp, err := s.currentHTTPClient().Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("upstream request failed")
		}
		if first == nil {
			first = resp.Header.Clone()
		}
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			resp.Body = newIdleBody(resp.Body, s.currentConfig().HFTimeout)
			return resp, first, nil
		}
		location := resp.Header.Get("Location")
		resp.Body.Close()
		if location == "" {
			return nil, first, fmt.Errorf("upstream redirect did not include a target")
		}
		if hops == 5 {
			return nil, first, fmt.Errorf("upstream redirect limit exceeded")
		}
		next, err := u.Parse(location)
		if err != nil {
			return nil, first, fmt.Errorf("invalid upstream redirect")
		}
		host := strings.ToLower(next.Hostname())
		if !s.allowedRedirectHost(host) || next.User != nil || next.Scheme != "https" && !(next.Scheme == "http" && configLoopback(next.Hostname())) {
			return nil, first, fmt.Errorf("upstream redirect host is not allowed")
		}
		if port := next.Port(); port != "" && port != "443" && !(port == "80" && configLoopback(next.Hostname())) {
			return nil, first, fmt.Errorf("unsafe upstream redirect port")
		}
		target = next.String()
	}
	return nil, first, fmt.Errorf("upstream redirect limit exceeded")
}

func configLoopback(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsLoopback()
	}
	return false
}
func newEgressDialer(allowLoopback bool, proxy *url.URL) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if proxy != nil {
			proxyScheme := strings.ToLower(proxy.Scheme)
			if strings.HasPrefix(proxyScheme, "socks4") || strings.HasPrefix(proxyScheme, "socks5") {
				return dialSOCKS(ctx, network, address, dialer, proxy, allowLoopback)
			}
			proxyPort := proxy.Port()
			if proxyPort == "" {
				proxyPort = "80"
				if proxyScheme == "https" {
					proxyPort = "443"
				}
			}
			proxyHost := proxy.Hostname()
			sameHost := strings.EqualFold(host, proxyHost)
			if hostIP, hostErr := netip.ParseAddr(host); hostErr == nil {
				if proxyIP, proxyErr := netip.ParseAddr(proxyHost); proxyErr == nil {
					sameHost = hostIP.Unmap() == proxyIP.Unmap()
				}
			}
			if !sameHost || port != proxyPort {
				return nil, fmt.Errorf("egress target does not match the configured Hugging Face proxy")
			}
			return dialConfiguredProxy(ctx, network, dialer, proxy)
		}
		checkAndDial := func(ip netip.Addr) (net.Conn, error) {
			ip = ip.Unmap()
			allowed := egressIPAllowed(ip, allowLoopback)
			if !allowed {
				return nil, fmt.Errorf("egress to non-public IP is blocked")
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			return checkAndDial(ip)
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, candidate := range addresses {
			ip, ok := netip.AddrFromSlice(candidate.IP)
			if !ok {
				continue
			}
			conn, dialErr := checkAndDial(ip)
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("upstream host resolved to no allowed IP addresses")
		}
		return nil, lastErr
	}
}
func newHFHTTPClient(cfg config.Config) *http.Client {
	base, _ := url.Parse(cfg.HFBaseURL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var proxy *url.URL
	if cfg.HFProxyURL != "" {
		proxy, _ = url.Parse(cfg.HFProxyURL)
		proxy.Scheme = strings.ToLower(proxy.Scheme)
		switch proxy.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxy)
		default:
			transport.Proxy = nil
		}
	} else {
		// Do not inherit HTTP(S)_PROXY from the process environment unless it
		// was explicitly configured for Hugging Face.
		transport.Proxy = nil
	}
	transport.DialContext = newEgressDialer(configLoopback(base.Hostname()), proxy)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.ResponseHeaderTimeout = cfg.HFTimeout
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (s *Server) allowedRedirectHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	base, _ := url.Parse(s.currentConfig().HFBaseURL)
	if strings.TrimSuffix(strings.ToLower(base.Hostname()), ".") == host {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil && !ip.IsGlobalUnicast() {
		return false
	}
	for _, allowed := range s.currentConfig().HFAllowedHosts {
		allowed = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(allowed)), ".")
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
		} else if strings.EqualFold(host, allowed) {
			return true
		}
	}
	return false
}
func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host":
		return true
	}
	return false
}

func (s *Server) findArtifact(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, repoID, commit, file string) (artifact, error) {
	var a artifact
	err := q.QueryRowContext(ctx, `SELECT a.id,a.repository_id,a.commit_sha,a.path,a.blob_id,b.storage_key,b.sha256,b.size_bytes,a.etag,b.content_type,a.last_modified FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.commit_sha=$2 AND a.path=$3`, repoID, commit, file).Scan(&a.ID, &a.Repository, &a.Commit, &a.Path, &a.BlobID, &a.StorageKey, &a.SHA256, &a.Size, &a.ETag, &a.ContentType, &a.LastMod)
	return a, err
}
func isNoRows(err error) bool { return err == sql.ErrNoRows }
func nullTime(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time
}

func (s *Server) setArtifactHeaders(w http.ResponseWriter, a artifact, upstream http.Header) {
	if a.ContentType != "" {
		w.Header().Set("Content-Type", a.ContentType)
	}
	if a.ETag != "" {
		w.Header().Set("ETag", a.ETag)
	}
	if a.LastMod.Valid {
		w.Header().Set("Last-Modified", a.LastMod.Time.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Repo-Commit", a.Commit)
	w.Header().Set("X-Cache", "HIT")
	for _, key := range []string{"Content-Disposition", "X-Linked-ETag", "X-Linked-Size", "X-Xet-Hash"} {
		if value := upstream.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if w.Header().Get("X-Linked-ETag") == "" {
		if digest := lfsSHA256(a.ETag); digest != "" {
			w.Header().Set("X-Linked-ETag", `"`+digest+`"`)
		}
	}
	if w.Header().Get("X-Linked-Size") == "" && w.Header().Get("X-Linked-ETag") != "" {
		w.Header().Set("X-Linked-Size", strconv.FormatInt(a.Size, 10))
	}
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, a artifact, cacheResult string) {
	setHeaders := func() { s.setArtifactHeaders(w, a, nil); w.Header().Set("X-Cache", cacheResult) }
	if ifNoneMatch(r.Header.Get("If-None-Match"), a.ETag) || (r.Header.Get("If-None-Match") == "" && a.LastMod.Valid && notModifiedSince(r.Header.Get("If-Modified-Since"), a.LastMod.Time)) {
		setHeaders()
		w.WriteHeader(http.StatusNotModified)
		return
	}
	start, end, partial, rangeOK := parseByteRange(r.Header.Get("Range"), a.Size)
	if r.Header.Get("If-Range") != "" && partial {
		if !ifRangeMatches(r.Header.Get("If-Range"), a) {
			partial = false
		}
	}
	if r.Header.Get("Range") != "" && rangeOK == false {
		w.Header().Set("X-Cache", cacheResult)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", a.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	byteRange := ""
	status := http.StatusOK
	if partial {
		byteRange = fmt.Sprintf("bytes=%d-%d", start, end)
		status = http.StatusPartialContent
	}
	if r.Method == http.MethodHead {
		setHeaders()
		if partial {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, a.Size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
		}
		w.WriteHeader(status)
		return
	}
	out, err := s.currentStorage().Get(r.Context(), a.StorageKey, byteRange)
	if err != nil {
		s.logger.Error("cache object read failed", "sha256", a.SHA256)
		writeProblem(w, r, 502, "Storage error", "The cached artifact is temporarily unavailable.")
		return
	}
	defer out.Body.Close()
	setHeaders()
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, a.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, out.Body)
}

func parseByteRange(value string, size int64) (int64, int64, bool, bool) {
	if value == "" {
		return 0, 0, false, true
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		// Unsupported units and multi-range requests are ignored when the
		// server does not implement multipart/byteranges.
		return 0, 0, false, true
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 || size <= 0 {
		return 0, 0, false, false
	}
	var start, end int64
	var err error
	if parts[0] == "" {
		suffix, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		if suffix > size {
			suffix = size
		}
		start = size - suffix
		end = size - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false, false
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				return 0, 0, false, false
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	return start, end, true, true
}
func ifNoneMatch(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		expected := strings.TrimPrefix(etag, "W/")
		if candidate == expected {
			return true
		}
	}
	return false
}
func notModifiedSince(header string, last time.Time) bool {
	if header == "" {
		return false
	}
	t, err := http.ParseTime(header)
	return err == nil && !last.Truncate(time.Second).After(t)
}
func ifRangeMatches(value string, a artifact) bool {
	if strings.HasPrefix(value, "W/") {
		return false
	}
	if strings.HasPrefix(value, "\"") {
		return value == a.ETag && !strings.HasPrefix(a.ETag, "W/")
	}
	if a.LastMod.Valid {
		if t, err := http.ParseTime(value); err == nil {
			return !a.LastMod.Time.Truncate(time.Second).After(t)
		}
	}
	return false
}

func (s *Server) hubFailure(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Warn("upstream request failed", "request_id", requestID(r.Context()), "class", fmt.Sprintf("%T", err))
	writeProblem(w, r, 502, "Hugging Face upstream unavailable", "The upstream request failed or returned invalid metadata.")
}
func (s *Server) hubStatus(w http.ResponseWriter, r *http.Request, status int, headers http.Header) {
	w.Header().Del("Content-Length")
	if status < 400 || status > 599 {
		status = 502
	}
	if retry := headers.Get("Retry-After"); retry != "" {
		if seconds, err := strconv.Atoi(retry); err == nil && seconds >= 0 && seconds <= 3600 {
			w.Header().Set("Retry-After", retry)
		}
	}
	for _, key := range []string{"X-Error-Code", "X-Error-Message", "X-Repo-Commit"} {
		if value := safeUpstreamHeader(headers.Get(key), 1024); value != "" {
			w.Header().Set(key, value)
		}
	}
	titles := map[int]string{400: "Bad request", 401: "Upstream authentication required", 403: "Upstream access denied", 404: "File or repository not found", 410: "Resource gone", 429: "Upstream rate limit", 500: "Upstream error", 502: "Upstream unavailable", 503: "Upstream unavailable", 504: "Upstream timeout"}
	title, ok := titles[status]
	if !ok {
		title = "Upstream error"
	}
	writeProblem(w, r, status, title, "Hugging Face returned an error for this resource.")
}

func safeUpstreamHeader(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func (s *Server) recordDownload(ctx context.Context, repoID, commit, file, status string, bytes int64, errorClass string) {
	_, err := s.db.SQL.ExecContext(ctx, "INSERT INTO downloads(id,repository_id,commit_sha,path,status,completed_at,bytes,error_class) VALUES($1,$2,$3,$4,$5,now(),$6,$7)", db.NewID(), repoID, commit, file, status, bytes, errorClass)
	if err != nil {
		s.logger.Warn("download event could not be recorded")
	}
}

func (s *Server) finishDownload(ctx context.Context, id string, bytes int64, errorClass string) {
	status := "miss"
	if errorClass != "" {
		status = "failed"
	}
	_, err := s.db.SQL.ExecContext(ctx, "UPDATE downloads SET status=$2,completed_at=now(),bytes=$3,error_class=$4 WHERE id=$1", id, status, bytes, errorClass)
	if err != nil {
		s.logger.Warn("download completion could not be recorded")
	}
}

func safeResolveQuery(values url.Values) url.Values {
	result := url.Values{}
	for _, key := range []string{"download", "raw"} {
		for _, value := range values[key] {
			if len(value) <= 16 {
				result.Add(key, value)
			}
		}
	}
	return result
}

func (s *Server) resolveCommit(ctx context.Context, parsed resolvePath) (string, error) {
	requestPath := "/api/" + parsed.RepoType + "/" + escapeRepoPath(parsed.RepoID) + "/revision/" + url.PathEscape(parsed.Revision)
	response, _, err := s.fetchHub(ctx, http.MethodGet, requestPath, nil, nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Hub revision lookup failed with status %d", response.StatusCode)
	}
	var payload struct {
		SHA       string
		Commit    string
		CommitSHA string
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("Hub revision response was invalid")
	}
	for _, value := range []string{payload.SHA, payload.Commit, payload.CommitSHA} {
		if validCommit(value) {
			return value, nil
		}
	}
	return "", fmt.Errorf("Hub did not return an immutable commit reference")
}
