package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/storage"
)

// fileResolution carries what the steps of resolveFile learn about one request.
type fileResolution struct {
	w         http.ResponseWriter
	r         *http.Request
	parsed    resolvePath
	principal *Principal
	policy    string
	repoPK    string

	// staleCommit is the last commit known for a branch or tag, used to answer
	// from the cache when the Hub cannot confirm the ref.
	staleCommit string
	commit      string

	// Filled by probeUpstream from the Hub's answer to the HEAD request.
	upstream     *http.Response
	firstHeaders http.Header
	meta         artifact
	sizeKnown    bool
	expectedLFS  string
}

func (res *fileResolution) closeUpstream() {
	if res.upstream != nil {
		res.upstream.Body.Close()
	}
}

// resolveFile serves /{repo}/resolve/{revision}/{path}. The steps, in order:
// access control, ref lookup, cache lookup, Hub probe, cache lookup by the
// resolved commit, then either a HEAD answer or a cache fill.
func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) {
	parsed, err := parseResolvePath(r.URL.EscapedPath())
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	p, authErr := s.authenticate(r)
	grant, err := s.repositoryAccessInfo(r.Context(), p, authErr, parsed.RepoType, parsed.RepoID)
	if err != nil {
		var access accessError
		if errors.As(err, &access) && access.status == http.StatusForbidden && access.title == "Repository not registered" &&
			s.inLocalNamespace(r.Context(), strings.SplitN(parsed.RepoID, "/", 2)[0]) && strings.Contains(parsed.RepoID, "/") {
			hfError(w, http.StatusNotFound, "RepoNotFound", "Repository not found")
			return
		}
		s.accessProblem(w, r, err)
		return
	}
	repoPK, policy := grant.ID, grant.Policy
	if refusal := s.formatRefusal(grant.AllowUnsafeFormats, parsed.FilePath); refusal != "" {
		s.countRefusedFormat("read")
		// A missing file, so clients that have an alternative (safetensors next
		// to a .bin) fall back to it instead of failing.
		hfError(w, http.StatusNotFound, "EntryNotFound", refusal)
		return
	}
	if grant.Origin == "local" {
		s.serveLocalFile(w, r, parsed, grant, p)
		return
	}
	r, w, releaseDownload, admitted := s.admitDownload(w, r, p)
	if !admitted {
		return
	}
	defer releaseDownload()
	s.logger.Debug("file request", "request_id", requestID(r.Context()), "method", r.Method, "repo_type", parsed.RepoType, "repo_id", parsed.RepoID, "revision", safeLogValue(parsed.Revision, 100), "path", safeLogValue(parsed.FilePath, 300), "policy", policy, "authenticated", p != nil)
	res := &fileResolution{w: w, r: r, parsed: parsed, principal: p, policy: policy, repoPK: repoPK}
	defer res.closeUpstream()

	resolved := s.knownCommit(res)
	if resolved != "" && s.serveCacheHit(res, resolved, "Unable to read cache metadata.") {
		return
	}
	if !s.probeUpstream(res) {
		return
	}
	if s.serveCacheHit(res, res.commit, "Unable to read cache metadata.") {
		return
	}
	if r.Method == http.MethodHead {
		s.answerHead(res)
		return
	}
	s.fillCache(res)
}

// knownCommit returns the commit the request names or a recent resolution of
// its ref, and remembers the last known commit for answering while the Hub is
// down.
func (s *Server) knownCommit(res *fileResolution) string {
	ctx, revision := res.r.Context(), res.parsed.Revision
	if validCommit(revision) {
		return revision
	}
	fresh, err := s.db.FreshRevision(ctx, res.repoPK, revision, s.currentConfig().CacheRefTTL)
	if err != nil {
		s.logger.Warn("revision lookup failed", "request_id", requestID(ctx))
	}
	if validCommit(fresh) {
		return fresh
	}
	// Remember the last known commit, however old, so the cache can answer if
	// the Hub cannot confirm the ref.
	last, err := s.db.LastKnownRevision(ctx, res.repoPK, revision)
	if err != nil {
		s.logger.Warn("last known revision lookup failed", "request_id", requestID(ctx))
	}
	if validCommit(last) {
		res.staleCommit = last
	}
	return ""
}

// serveCacheHit answers from the cache when the file is cached for commit. It
// reports whether the request was handled, including by an error response.
func (s *Server) serveCacheHit(res *fileResolution, commit, errorDetail string) bool {
	cached, err := s.findArtifact(res.r.Context(), res.repoPK, commit, res.parsed.FilePath)
	if err == nil {
		s.touchArtifact(res.r.Context(), cached.ID, res.r.Method == http.MethodGet)
		s.logger.Debug("cache hit", "request_id", requestID(res.r.Context()), "repo_id", res.parsed.RepoID, "path", safeLogValue(res.parsed.FilePath, 300), "commit", commit, "bytes", cached.Size)
		recordHit := func() {
			s.recordDownload(res.r.Context(), res.repoPK, commit, res.parsed.FilePath, "hit", cached.Size, "")
		}
		if s.serveOrAcknowledgePreloadReporting(res.w, res.r, cached, "HIT", recordHit) {
			// The catalog lists a file whose object is gone from the bucket (it was
			// deleted there, or lost to a race). Drop the entry and go on as for a
			// miss: the file is fetched again and its object restored, instead of
			// answering an error until an operator purges it by hand.
			return !s.healMissingObject(res, cached, commit)
		}
		return true
	}
	if !isNoRows(err) {
		writeProblem(res.w, res.r, http.StatusInternalServerError, "Cache metadata error", errorDetail)
		return true
	}
	return false
}

// healMissingObject removes the catalog entry of a file whose object is missing
// from the bucket, so that the request that found out refills it.
// It reports whether the entry was dropped; when it was not, an error response
// has been written.
func (s *Server) healMissingObject(res *fileResolution, cached artifact, commit string) bool {
	ctx := res.r.Context()
	err := s.db.PurgeArtifact(ctx, cached.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		s.logger.Error("a catalog entry whose object is missing could not be dropped", "request_id", requestID(ctx), "repo_id", res.parsed.RepoID, "path", safeLogValue(res.parsed.FilePath, 300), "error", safeLogValue(err.Error(), 200))
		writeProblem(res.w, res.r, http.StatusBadGateway, "Storage error", "The cached artifact is temporarily unavailable.")
		return false
	}
	s.logger.Warn("the object of a cached file was missing; its entry was dropped and the file is fetched again", "request_id", requestID(ctx), "repo_id", res.parsed.RepoID, "path", safeLogValue(res.parsed.FilePath, 300), "commit", commit, "sha256", cached.SHA256)
	if s.metrics != nil {
		s.metrics.add("hfproxy_cache_object_repairs_total", nil, 1)
	}
	return true
}

// touchArtifact records an access; a failure only costs eviction accuracy, so it
// is logged and not surfaced.
func (s *Server) touchArtifact(ctx context.Context, id string, download bool) {
	if err := s.db.TouchArtifact(ctx, id, download, s.currentConfig().CacheEviction.HalfLife()); err != nil {
		s.logger.Warn("artifact access time could not be recorded", "request_id", requestID(ctx))
	}
}

// probeUpstream asks the Hub for the file's headers and fills in the commit and
// metadata. It reports false after writing a response.
func (s *Server) probeUpstream(res *fileResolution) bool {
	w, r, parsed := res.w, res.r, res.parsed
	requestURL := upstreamResolvePath(parsed)
	negativeKey := res.repoPK + "|" + parsed.Revision + "|" + parsed.FilePath
	if status, headers, ok := s.negativeLookup(negativeKey); ok {
		if s.metrics != nil {
			s.metrics.cacheDecision(r.Context(), "bypass")
		}
		s.hubStatus(w, r, status, headers)
		return false
	}
	upstream, firstHeaders, err := s.fetchHub(r.Context(), http.MethodHead, requestURL, safeResolveQuery(r.URL.Query()), nil)
	if err != nil {
		if s.serveStale(w, r, res.repoPK, res.staleCommit, parsed) {
			return false
		}
		s.hubFailure(w, r, err)
		return false
	}
	res.upstream, res.firstHeaders = upstream, firstHeaders
	if upstream.StatusCode == http.StatusTooManyRequests || upstream.StatusCode >= 500 {
		s.logger.Warn("the Hub answered with an error", "request_id", requestID(r.Context()), "repo_id", parsed.RepoID, "path", safeLogValue(parsed.FilePath, 300), "status", upstream.StatusCode)
	} else {
		s.logger.Debug("the Hub answered", "request_id", requestID(r.Context()), "repo_id", parsed.RepoID, "path", safeLogValue(parsed.FilePath, 300), "status", upstream.StatusCode)
	}
	if upstream.StatusCode == http.StatusTooManyRequests || upstream.StatusCode >= 500 {
		if s.serveStale(w, r, res.repoPK, res.staleCommit, parsed) {
			return false
		}
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if upstream.StatusCode == http.StatusNotFound || upstream.StatusCode == http.StatusGone {
			s.negativeStore(negativeKey, upstream.StatusCode, upstream.Header)
		}
		s.hubStatus(w, r, upstream.StatusCode, upstream.Header)
		return false
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
			return false
		}
	}
	if !validCommit(parsed.Revision) {
		if err := s.db.SaveRevision(r.Context(), res.repoPK, parsed.Revision, commit); err != nil {
			s.logger.Warn("resolved revision could not be saved", "request_id", requestID(r.Context()))
		}
	}
	res.commit = commit
	res.meta, res.sizeKnown = upstreamMetadata(res.repoPK, commit, parsed.FilePath, upstream, firstHeaders)
	res.expectedLFS = lfsSHA256(firstHeaders.Get("X-Linked-ETag"))
	return true
}

// upstreamMetadata builds the artifact description from the Hub's HEAD answer.
// The bool reports whether the size is known before downloading.
func upstreamMetadata(repoPK, commit, filePath string, upstream *http.Response, firstHeaders http.Header) (artifact, bool) {
	meta := artifact{Repository: repoPK, Commit: commit, Path: filePath, ETag: upstream.Header.Get("ETag"), ContentType: upstream.Header.Get("Content-Type")}
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
	return meta, sizeKnown
}

// answerHead answers a HEAD request for a file that is not cached yet.
func (s *Server) answerHead(res *fileResolution) {
	w, r, meta := res.w, res.r, res.meta
	if ifNoneMatch(r.Header.Get("If-None-Match"), meta.ETag) || (r.Header.Get("If-None-Match") == "" && meta.LastMod.Valid && notModifiedSince(r.Header.Get("If-Modified-Since"), meta.LastMod.Time)) {
		s.setArtifactHeaders(w, meta, res.upstream.Header)
		w.Header().Set("X-Repo-Commit", res.commit)
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.setArtifactHeaders(w, meta, res.upstream.Header)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Repo-Commit", res.commit)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
}

// cacheFill is the state of one download from the Hub into the cache.
type cacheFill struct {
	*fileResolution
	ctx         context.Context // lifetime of the fill (see fillCache)
	downloadID  string
	objectStore *storage.S3
	tmp         *os.File
	body        *http.Response
	size        int64
	digest      string
	resume      *downloadResume
	streamed    bool
	started     time.Time
}

// fillCache downloads the file from the Hub, stores it in object storage and
// records it, answering the request with the result. Only one request per file
// fills the cache; the others stream the response uncached or wait for it.
func (s *Server) fillCache(res *fileResolution) {
	w, r := res.w, res.r
	missKey := res.repoPK + "|" + res.commit + "|" + res.parsed.FilePath
	leader, done := s.joinCacheMiss(missKey)
	if r.Method == http.MethodGet && res.expectedLFS == "" && r.Header.Get("Range") != "" {
		if leader {
			defer s.finishCacheMiss(missKey)
		}
		s.streamUncachedMiss(w, r, res.parsed, res.meta, res.commit, "")
		return
	}
	if !leader {
		if r.Method == http.MethodGet && res.expectedLFS == "" {
			s.streamUncachedMiss(w, r, res.parsed, res.meta, res.commit, "")
			return
		}
		select {
		case <-done:
			s.resolveFile(w, r)
		case <-r.Context().Done():
		}
		return
	}
	defer s.finishCacheMiss(missKey)
	if !s.admitFill(res) {
		return
	}
	releaseSlot, slotErr := s.acquireFillSlot(r.Context())
	if slotErr != nil {
		if errors.Is(slotErr, errFillBusy) {
			w.Header().Set("Retry-After", "5")
			writeProblem(w, r, http.StatusServiceUnavailable, "Cache fill capacity exhausted", "Too many files are being fetched right now; retry shortly.")
		}
		return
	}
	defer releaseSlot()

	// A streamed fill belongs to the proxy: the client may hang up as soon as it
	// has the last byte (curl, a pooled connection that closes), and that must not
	// abort the storing and recording that follow. Fills that are not streamed
	// keep the request's lifetime, so a client that leaves stops the download and
	// the next attempt resumes from the stored blocks.
	fillCtx := r.Context()
	if shouldStreamCacheMiss(r, res.expectedLFS) {
		fillCtx = context.WithoutCancel(fillCtx)
	}
	fill := &cacheFill{fileResolution: res, ctx: fillCtx, downloadID: db.NewID(), objectStore: s.currentStorage()}
	if err := s.db.StartDownload(fill.ctx, fill.downloadID, res.repoPK, res.commit, res.parsed.FilePath, dbCaller(callerOf(fill.ctx))); err != nil {
		s.logger.Warn("download start could not be recorded", "request_id", requestID(fill.ctx))
	}
	if res.meta.Size > s.currentConfig().CacheMaxFileSize {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "size_limit")
		writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
		return
	}
	if res.sizeKnown && res.meta.Size > 0 {
		releaseStaging, stagingErr := s.reserveStaging(res.meta.Size)
		if stagingErr != nil {
			s.finishDownload(fill.ctx, fill.downloadID, 0, "staging_space")
			w.Header().Set("Retry-After", "30")
			writeProblem(w, r, http.StatusInsufficientStorage, "Insufficient staging space", "The proxy does not have enough free disk space to stage this file right now.")
			return
		}
		defer releaseStaging()
	}
	// A database lock per file makes fills exclusive across all instances that
	// share the database: the others wait here, then find the file cached.
	// The wait is bounded: behind a slow fill on another instance it would hold
	// this fill slot, a staging reservation and a connection for hours. After the
	// bound the client is served straight from the Hub instead.
	lockCtx, stopLockWait := context.WithTimeout(fill.ctx, fillLockWait)
	unlockFill, lockErr := s.lockDownloadResume(lockCtx, res.repoPK, res.commit, res.parsed.FilePath)
	waited := lockCtx.Err() != nil
	stopLockWait()
	if lockErr != nil && waited && fill.ctx.Err() == nil {
		s.logger.Info("another instance is filling this file; serving it from the Hub meanwhile", "request_id", requestID(fill.ctx), "wait", fillLockWait.String())
		s.finishDownload(fill.ctx, fill.downloadID, 0, "resume_lock_wait")
		if r.Method == http.MethodGet && res.expectedLFS == "" {
			s.streamUncachedMiss(w, r, res.parsed, res.meta, res.commit, "")
			return
		}
		w.Header().Set("Retry-After", "30")
		writeProblem(w, r, http.StatusServiceUnavailable, "File is being cached", "Another instance is still fetching this file; retry shortly.")
		return
	}
	if lockErr != nil {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "resume_lock")
		writeProblem(w, r, http.StatusInternalServerError, "Cache metadata error", "Unable to coordinate the cache fill.")
		return
	}
	defer unlockFill()
	if s.serveCacheHit(res, res.commit, "Unable to recheck cache metadata.") {
		return
	}
	defer func() {
		if fill.body != nil {
			fill.body.Body.Close()
		}
		if fill.tmp != nil {
			name := fill.tmp.Name()
			_ = fill.tmp.Close()
			_ = os.Remove(name)
		}
	}()
	fill.started = time.Now()
	var ok bool
	if res.sizeKnown {
		ok = s.downloadKnownSize(fill)
	} else {
		ok = s.downloadUnknownSize(fill)
	}
	if !ok {
		return
	}
	if res.expectedLFS != "" && fill.digest != res.expectedLFS {
		if fill.resume != nil {
			_ = s.resetDownloadResume(fill.ctx, fill.objectStore, fill.resume, fill.resume.ETag, fill.resume.TotalSize, fill.resume.ContentType)
		}
		s.finishDownload(fill.ctx, fill.downloadID, fill.size, "integrity")
		writeProblem(w, r, http.StatusBadGateway, "Integrity check failed", "The downloaded file did not match the Hub's advertised SHA-256.")
		return
	}
	s.publishDownload(fill)
}

func dbCaller(info requestInfo) db.Caller {
	return db.Caller{UserID: info.UserID, TokenID: info.TokenID, ClientIP: info.ClientIP}
}

// downloadKnownSize fetches a file whose size the Hub announced, in resumable
// blocks. It reports false after writing a response.
func (s *Server) downloadKnownSize(fill *cacheFill) bool {
	w, r, res := fill.w, fill.r, fill.fileResolution
	resumeETag := res.upstream.Header.Get("ETag")
	if resumeETag == "" {
		resumeETag = res.firstHeaders.Get("ETag")
	}
	var stream io.Writer
	if shouldStreamCacheMiss(r, res.expectedLFS) {
		s.setArtifactHeaders(w, res.meta, res.firstHeaders)
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Repo-Commit", res.commit)
		w.Header().Set("Content-Length", strconv.FormatInt(res.meta.Size, 10))
		w.WriteHeader(http.StatusOK)
		stream = asStreamingWriter(w)
		fill.streamed = true
	}
	var err error
	fill.tmp, fill.digest, fill.resume, err = s.downloadResumable(fill.ctx, fill.objectStore, fill.downloadID, res.repoPK, res.commit, res.parsed, res.meta.Size, resumeETag, res.meta.ContentType, stream)
	fill.size = res.meta.Size
	if err != nil {
		completed := int64(0)
		if fill.resume != nil {
			completed = fill.resume.CompletedBytes
		}
		s.finishDownload(fill.ctx, fill.downloadID, completed, "upstream_read")
		var statusErr *upstreamStatusError
		if errors.As(err, &statusErr) {
			s.hubStatus(w, r, statusErr.status, statusErr.header)
			return false
		}
		writeProblem(w, r, http.StatusBadGateway, "Upstream transfer failed", "The file could not be completed. Retry the preload to continue from the last stored block.")
		return false
	}
	return true
}

// downloadUnknownSize fetches a file of unknown size in one pass, enforcing the
// size limit while reading. It reports false after writing a response.
func (s *Server) downloadUnknownSize(fill *cacheFill) bool {
	w, r, res := fill.w, fill.r, fill.fileResolution
	fetchPath := upstreamResolvePath(resolvePath{RepoType: res.parsed.RepoType, RepoID: res.parsed.RepoID, Revision: res.commit, FilePath: res.parsed.FilePath})
	body, headers, err := s.fetchHub(fill.ctx, http.MethodGet, fetchPath, safeResolveQuery(r.URL.Query()), nil)
	if err != nil {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "upstream")
		s.hubFailure(w, r, err)
		return false
	}
	fill.body = body
	if body.StatusCode < 200 || body.StatusCode >= 300 {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "upstream_status")
		s.hubStatus(w, r, body.StatusCode, body.Header)
		return false
	}
	actualCommit := headers.Get("X-Repo-Commit")
	if actualCommit == "" {
		actualCommit = body.Header.Get("X-Repo-Commit")
	}
	if actualCommit != "" && actualCommit != res.commit {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "revision_changed")
		writeProblem(w, r, http.StatusConflict, "Revision changed", "The requested branch moved during the download; retry against the resolved revision.")
		return false
	}
	maxSize := s.currentConfig().CacheMaxFileSize
	if body.ContentLength > maxSize {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "size_limit")
		writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
		return false
	}
	fill.tmp, err = s.createStagingFile("hfproxy-staging-*")
	if err != nil {
		s.finishDownload(fill.ctx, fill.downloadID, 0, "staging")
		writeProblem(w, r, 500, "Cache error", "Unable to stage the upstream file.")
		return false
	}
	hash := sha256.New()
	limited := io.LimitReader(body.Body, maxSize+1)
	destination := io.Writer(io.MultiWriter(fill.tmp, hash))
	if shouldStreamCacheMiss(r, res.expectedLFS) {
		s.setArtifactHeaders(w, res.meta, headers)
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Repo-Commit", res.commit)
		if body.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(body.ContentLength, 10))
		}
		w.WriteHeader(http.StatusOK)
		destination = io.MultiWriter(fill.tmp, hash, asStreamingWriter(w))
		fill.streamed = true
	}
	progress := &downloadProgressWriter{ctx: fill.ctx, store: s.db, id: fill.downloadID, destination: destination, lastPersist: time.Now()}
	fill.size, err = io.Copy(progress, limited)
	if err != nil {
		s.finishDownload(fill.ctx, fill.downloadID, fill.size, "upstream_read")
		writeStreamAwareProblem(w, r, fill.streamed, 502, "Upstream transfer failed", "The Hub response could not be completely downloaded.")
		return false
	}
	if fill.size > maxSize {
		s.finishDownload(fill.ctx, fill.downloadID, fill.size, "size_limit")
		writeStreamAwareProblem(w, r, fill.streamed, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
		return false
	}
	fill.digest = hex.EncodeToString(hash.Sum(nil))
	return true
}

// publishmentMessages maps a failed publication step to the client message.
var publishmentMessages = map[string]string{
	"begin":           "Unable to open cache metadata transaction.",
	"read_blob":       "Unable to read blob metadata.",
	"insert_blob":     "Unable to publish cache metadata.",
	"restore_blob":    "Unable to restore cache metadata.",
	"insert_artifact": "Unable to publish artifact metadata.",
	"commit":          "Unable to commit artifact metadata.",
}

// storageStepError marks a failure to check or restore the object in storage
// while the blob row is locked.
type storageStepError struct {
	class   string
	message string
	err     error
}

func (e *storageStepError) Error() string { return e.message }
func (e *storageStepError) Unwrap() error { return e.err }

// publishDownload uploads the staged file to object storage, records it in the
// database and answers the request.
func (s *Server) publishDownload(fill *cacheFill) {
	w, r, res := fill.w, fill.r, fill.fileResolution
	objectStore := fill.objectStore
	key := objectStore.Key(fill.digest)
	contentType := res.meta.ContentType
	if fill.body != nil && fill.body.Header.Get("Content-Type") != "" {
		contentType = fill.body.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(res.parsed.FilePath))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// Once the upstream body is complete, cache publication belongs to the
	// proxy even if the client closes its connection after receiving the body.
	publishCtx, cancelPublish := cachePublishContext(fill.ctx)
	defer cancelPublish()
	fail := func(class string, cause error, status int, title, detail string) {
		s.logger.Error("cache publication failed", "request_id", requestID(r.Context()), "repo_id", res.parsed.RepoID, "path", safeLogValue(res.parsed.FilePath, 300), "step", class, "error", safeLogValue(cause.Error(), 300))
		s.finishDownload(publishCtx, fill.downloadID, fill.size, class)
		writeStreamAwareProblem(w, r, fill.streamed, status, title, detail)
	}
	if fill.resume == nil {
		if err := fill.tmp.Sync(); err != nil {
			fail("staging", err, 500, "Cache error", "Unable to finalize the staged file.")
			return
		}
	}
	// S3 calls must not run inside a database transaction: each transaction
	// reserves a pool connection, and slow uploads can exhaust the pool while
	// their progress writers need connections of their own.
	blobExists, err := s.db.BlobExists(publishCtx, fill.digest)
	if err != nil {
		fail("metadata", err, 500, "Cache metadata error", "Unable to check blob metadata.")
		return
	}
	if !blobExists {
		if err := objectStore.UploadFile(publishCtx, fill.tmp, key, contentType, fill.size); err != nil {
			fail("storage", err, 502, "Storage error", "The downloaded file could not be published to object storage.")
			return
		}
	}
	// A fill's duration prices the next miss only when it fetched the whole file.
	fillSeconds := 0.0
	if !blobExists && (fill.resume == nil || fill.resume.ResumedBytes == 0) {
		fillSeconds = time.Since(fill.started).Seconds()
	}
	meta := res.meta
	meta.Size = fill.size
	meta.ContentType = contentType
	if fill.body != nil {
		if meta.ETag == "" {
			meta.ETag = fill.body.Header.Get("ETag")
		}
		if lm := fill.body.Header.Get("Last-Modified"); lm != "" {
			if t, e := http.ParseTime(lm); e == nil {
				meta.LastMod = sql.NullTime{Time: t, Valid: true}
			}
		}
	}
	err = s.db.PublishArtifact(publishCtx, db.PublishInput{
		RepoID: res.repoPK, Commit: res.commit, Path: res.parsed.FilePath, SHA256: fill.digest, Size: fill.size,
		StorageKey: key, ContentType: contentType, ETag: meta.ETag, LastMod: meta.LastMod,
		// Whether this fill put the object there, or relied on a blob row that may
		// have been collected since it was looked at.
		ObjectStored: !blobExists,
		FillSeconds:  fillSeconds,
	}, func(ctx context.Context, existingKey string) error {
		if _, headErr := objectStore.Head(ctx, existingKey); headErr != nil {
			if !storage.IsNotFound(headErr) {
				return &storageStepError{"storage_check", "The existing cached object could not be checked.", headErr}
			}
			if uploadErr := objectStore.UploadFile(ctx, fill.tmp, existingKey, contentType, fill.size); uploadErr != nil {
				return &storageStepError{"storage", "The missing cached object could not be restored.", uploadErr}
			}
		}
		return nil
	})
	if err != nil {
		var stepErr *storageStepError
		var publishErr *db.PublishError
		switch {
		case errors.As(err, &stepErr):
			fail(stepErr.class, err, http.StatusBadGateway, "Storage error", stepErr.message)
		case errors.As(err, &publishErr):
			fail("metadata", err, 500, "Cache metadata error", publishmentMessages[publishErr.Stage])
		default:
			fail("metadata", err, 500, "Cache metadata error", "Unable to publish artifact metadata.")
		}
		return
	}
	if fill.resume != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.finishDownloadResume(cleanupCtx, objectStore, fill.resume)
		cancel()
	}
	s.finishDownload(publishCtx, fill.downloadID, fill.size, "")
	s.logger.Info("file cached", "request_id", requestID(r.Context()), "repo_id", res.parsed.RepoID, "path", safeLogValue(res.parsed.FilePath, 300), "commit", res.commit, "bytes", fill.size, "streamed", fill.streamed, "resumed", fill.resume != nil)
	if fill.streamed {
		return
	}
	cached, err := s.findArtifact(publishCtx, res.repoPK, res.commit, res.parsed.FilePath)
	if err != nil {
		writeProblem(w, r, 500, "Cache metadata error", "The artifact was stored but could not be read back.")
		return
	}
	s.serveOrAcknowledgePreload(w, r, cached, "MISS")
}
