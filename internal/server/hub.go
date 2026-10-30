package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/hfproxy/hfproxy/internal/db"
)

type artifact = db.Artifact

func shouldStreamCacheMiss(r *http.Request, expectedLFS string) bool {
	return expectedLFS == "" && !isPreloadFetch(r.Context()) && r.Method == http.MethodGet &&
		r.Header.Get("Range") == "" && r.Header.Get("If-Range") == "" &&
		r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") == ""
}

func cachePublishContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 30*time.Minute)
}

func writeStreamAwareProblem(w http.ResponseWriter, r *http.Request, streamed bool, status int, title, detail string) {
	if streamed {
		panic(http.ErrAbortHandler)
	}
	writeProblem(w, r, status, title, detail)
}

type resolvePath struct{ RepoType, RepoID, Revision, FilePath string }

type downloadProgressWriter struct {
	ctx         context.Context
	store       *db.Store
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
		_ = w.store.SetDownloadBytes(w.ctx, w.id, w.baseBytes+w.bytes)
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

// serveStale answers from the cache when the Hub cannot confirm a branch or tag
// that was resolved before. It only applies when the last known commit has the
// file cached, and never to preload fetches, which exist to refresh the cache.
func (s *Server) serveStale(w http.ResponseWriter, r *http.Request, repoPK, commit string, parsed resolvePath) bool {
	if commit == "" || isPreloadFetch(r.Context()) {
		return false
	}
	cached, err := s.findArtifact(r.Context(), repoPK, commit, parsed.FilePath)
	if err != nil {
		return false
	}
	s.logger.Warn("serving cached file while the Hub is unavailable", "request_id", requestID(r.Context()))
	s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
	s.serveOrAcknowledgePreload(w, r, cached, "STALE")
	return true
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

// repositoryGrant is what the access check found out about a repository the
// caller may read.
type repositoryGrant struct {
	ID, Policy, Origin, OwnerID string
	AllowUnsafeFormats          bool
}

func (s *Server) repositoryAccess(ctx context.Context, p *Principal, authErr error, repoType, repoID string) (string, string, error) {
	grant, err := s.repositoryAccessInfo(ctx, p, authErr, repoType, repoID)
	return grant.ID, grant.Policy, err
}

func (s *Server) repositoryAccessInfo(ctx context.Context, p *Principal, authErr error, repoType, repoID string) (repositoryGrant, error) {
	repo, err := s.db.FindRepository(ctx, repoType, repoID)
	if errors.Is(err, db.ErrNotFound) {
		return repositoryGrant{}, accessError{http.StatusForbidden, "Repository not registered", "This repository has no local access policy."}
	}
	if err != nil {
		return repositoryGrant{}, err
	}
	id, policy := repo.ID, repo.Policy
	granted := repositoryGrant{ID: id, Policy: policy, Origin: repo.Origin, OwnerID: repo.OwnerID, AllowUnsafeFormats: repo.AllowUnsafeFormats}
	if p == nil {
		if authErr != errNoCredentials || !s.currentConfig().AnonymousPublicRead || policy != "public" {
			return repositoryGrant{}, accessError{http.StatusUnauthorized, "Authentication required", "Sign in to access this repository."}
		}
		return granted, nil
	}
	if p.Role == "admin" {
		return granted, nil
	}
	switch policy {
	case "public":
		return granted, nil
	case "admin_only":
		return repositoryGrant{}, accessError{http.StatusForbidden, "Forbidden", "This repository is restricted to administrators."}
	case "allowlist":
		allowed, err := s.db.HasReadGrant(ctx, id, p.ID)
		if err != nil {
			return repositoryGrant{}, err
		}
		// The person who published a repository can always read it.
		if !allowed && (repo.OwnerID == "" || repo.OwnerID != p.ID) {
			return repositoryGrant{}, accessError{http.StatusForbidden, "Forbidden", "You do not have read access to this repository."}
		}
		return granted, nil
	}
	return repositoryGrant{}, accessError{http.StatusForbidden, "Forbidden", "Repository access denied."}
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

// streamUncachedMiss serves requests that cannot wait for a cache fill, such as
// cold range requests and followers of an in-progress fill, and the files the
// admission policy does not keep. LFS objects with an advertised digest stay on
// the verified cache-fill path. With an expected SHA-256 and a whole-file
// answer, the bytes are checked as they pass and the connection is cut on a
// mismatch, as a streamed fill does: the client must not keep a wrong file that
// looks complete.
func (s *Server) streamUncachedMiss(w http.ResponseWriter, r *http.Request, parsed resolvePath, meta artifact, commit, expectedSHA string) {
	headers := make(http.Header)
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	pinned := resolvePath{RepoType: parsed.RepoType, RepoID: parsed.RepoID, Revision: commit, FilePath: parsed.FilePath}
	response, _, err := s.fetchHub(r.Context(), http.MethodGet, upstreamResolvePath(pinned), safeResolveQuery(r.URL.Query()), headers)
	if err != nil {
		s.hubFailure(w, r, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.hubStatus(w, r, response.StatusCode, response.Header)
		return
	}
	s.setArtifactHeaders(w, meta, response.Header)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Repo-Commit", commit)
	for _, name := range []string{"Content-Range", "Content-Encoding"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if response.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	}
	w.WriteHeader(response.StatusCode)
	if expectedSHA == "" || response.StatusCode != http.StatusOK {
		if _, err := io.Copy(w, response.Body); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, hash), response.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedSHA {
		s.logger.Warn("a file streamed without caching did not match the Hub's SHA-256", "request_id", requestID(r.Context()), "repo_id", parsed.RepoID, "path", safeLogValue(parsed.FilePath, 300))
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) findArtifact(ctx context.Context, repoID, commit, file string) (artifact, error) {
	ctx, span := otel.Tracer("github.com/hfproxy/hfproxy/internal/server").Start(ctx, "cache.lookup", trace.WithAttributes(attribute.String("cache.operation", "artifact_lookup")))
	defer span.End()
	started := time.Now()
	a, err := s.db.FindArtifact(ctx, repoID, commit, file)
	if s.metrics != nil {
		result := "hit"
		if isNoRows(err) {
			result = "miss"
		} else if err != nil {
			result = "error"
			span.SetStatus(codes.Error, "cache lookup failed")
		}
		span.SetAttributes(attribute.String("cache.result", result))
		s.metrics.cacheDecision(ctx, result)
		s.metrics.observe("hfproxy_cache_lookup_duration_seconds", nil, time.Since(started).Seconds())
	}
	return a, err
}
func isNoRows(err error) bool { return err == sql.ErrNoRows }
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
	event := db.DownloadEvent{RepoID: repoID, Commit: commit, Path: file, Status: status, Bytes: bytes, ErrorClass: errorClass, Caller: dbCaller(callerOf(ctx))}
	if status == "hit" && s.enqueueHit(event) {
		return
	}
	ctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if err := s.db.RecordDownloads(ctx, []db.DownloadEvent{event}); err != nil {
		s.logger.Warn("download event could not be recorded")
	}
}

// bookkeepingContext is for the small writes that record what happened (a
// download finished, a hit): a client that disconnects must not make them fail
// and leave a download "in progress" forever.
func bookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// callerOf returns who is making the request in ctx, or an empty value for
// background work such as preloads.
func callerOf(ctx context.Context) requestInfo {
	if info := requestInfoFrom(ctx); info != nil {
		return *info
	}
	return requestInfo{}
}

func (s *Server) finishDownload(ctx context.Context, id string, bytes int64, errorClass string) {
	ctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if errorClass != "" {
		s.logger.Warn("download did not complete", "request_id", requestID(ctx), "download_id", id, "reason", errorClass, "bytes", bytes)
	}
	if err := s.db.FinishDownload(ctx, id, bytes, errorClass); err != nil {
		s.logger.Warn("download completion could not be recorded", "request_id", requestID(ctx), "download_id", id, "error", safeLogValue(err.Error(), 200))
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
