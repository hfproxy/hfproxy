package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/db"
)

const (
	preloadPageSize  = 1000
	preloadMaxFiles  = 10000
	preloadMaxPages  = preloadMaxFiles / preloadPageSize
	preloadPageLimit = 16 << 20
)

type localPrincipalContextKey struct{}
type preloadFetchContextKey struct{}

type preloadJob struct {
	ID             string    `json:"id"`
	RepoType       string    `json:"repo_type"`
	RepoID         string    `json:"repo_id"`
	CommitSHA      string    `json:"commit_sha,omitempty"`
	Status         string    `json:"status"`
	FilesTotal     int       `json:"files_total"`
	FilesProcessed int       `json:"files_processed"`
	FailedFiles    int       `json:"failed_files"`
	Error          string    `json:"error,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type hubTreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

func (s *Server) startRepoPreload(w http.ResponseWriter, r *http.Request, repositoryID string) {
	principal, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	var repoType, repoID string
	err := s.db.SQL.QueryRowContext(r.Context(), `SELECT repo_type,repo_id FROM repositories WHERE id=$1 AND deleted_at IS NULL`, repositoryID).Scan(&repoType, &repoID)
	if errors.Is(err, sql.ErrNoRows) {
		writeProblem(w, r, http.StatusNotFound, "Repository not found", "The repository does not exist.")
		return
	}
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the repository.")
		return
	}

	now := time.Now().UTC()
	job := &preloadJob{ID: db.NewID(), RepoType: repoType, RepoID: repoID, Status: "queued", StartedAt: now, UpdatedAt: now}
	s.preloadsMu.Lock()
	if s.preloads == nil {
		s.preloads = make(map[string]*preloadJob)
	}
	for _, existing := range s.preloads {
		if existing.RepoType == repoType && existing.RepoID == repoID && (existing.Status == "queued" || existing.Status == "running") {
			snapshot := *existing
			s.preloadsMu.Unlock()
			writeJSON(w, http.StatusAccepted, snapshot)
			return
		}
	}
	s.preloads[job.ID] = job
	s.preloadsMu.Unlock()

	s.audit(r.Context(), principal.ID, "repository.preload.started", "repository", repositoryID, map[string]any{
		"job_id": job.ID, "repo_type": repoType, "repo_id": repoID,
	})
	snapshot := *job
	go s.runRepoPreload(job.ID, repositoryID, repoType, repoID, principal)
	writeJSON(w, http.StatusAccepted, snapshot)
}

func (s *Server) runRepoPreload(jobID, repositoryPK, repoType, repoID string, principal *Principal) {
	s.preloadSlots <- struct{}{}
	defer func() { <-s.preloadSlots }()
	ctx := context.Background()
	s.updatePreload(jobID, func(job *preloadJob) { job.Status = "running" })
	files, commit, err := s.listHubFiles(ctx, repoType, repoID, "main")
	if err != nil {
		s.updatePreload(jobID, func(job *preloadJob) {
			job.Status = "failed"
			job.Error = err.Error()
		})
		s.recordPreloadCompletion(ctx, principal.ID, repositoryPK, jobID, repoType, repoID, "failed", "", 0, 0, 0, err.Error())
		return
	}
	if !validCommit(commit) {
		commit, err = s.resolveCommit(ctx, resolvePath{RepoType: repoType, RepoID: repoID, Revision: "main"})
		if err != nil {
			message := "Unable to resolve the repository's main revision from Hugging Face."
			s.updatePreload(jobID, func(job *preloadJob) {
				job.Status = "failed"
				job.FilesTotal = len(files)
				job.Error = message
			})
			s.recordPreloadCompletion(ctx, principal.ID, repositoryPK, jobID, repoType, repoID, "failed", "", len(files), 0, len(files), message)
			return
		}
	}
	s.updatePreload(jobID, func(job *preloadJob) {
		job.FilesTotal = len(files)
		job.CommitSHA = commit
	})

	processed, failed := 0, 0
	for _, file := range files {
		requestPath := "/" + repoType + "/" + escapeRepoPath(repoID) + "/resolve/" + url.PathEscape(commit) + "/" + escapeFilePath(file)
		req := httptest.NewRequest(http.MethodGet, requestPath, nil)
		requestContext := context.WithValue(ctx, localPrincipalContextKey{}, principal)
		requestContext = context.WithValue(requestContext, preloadFetchContextKey{}, true)
		req = req.WithContext(requestContext)
		result := httptest.NewRecorder()
		s.resolveFile(result, req)
		requestFailed := result.Code < http.StatusOK || result.Code >= http.StatusMultipleChoices
		processed++
		if requestFailed {
			failed++
		}
		s.updatePreload(jobID, func(job *preloadJob) {
			job.FilesProcessed++
			if requestFailed {
				job.FailedFiles++
				if job.Error == "" {
					job.Error = fmt.Sprintf("Failed to download %s (HTTP %d).", file, result.Code)
				}
			}
		})
	}
	var status, message string
	s.updatePreload(jobID, func(job *preloadJob) {
		if job.FailedFiles == 0 {
			job.Status = "completed"
		} else {
			job.Status = "completed_with_errors"
		}
		status, message = job.Status, job.Error
	})
	s.recordPreloadCompletion(ctx, principal.ID, repositoryPK, jobID, repoType, repoID, status, commit, len(files), processed, failed, message)
}

func (s *Server) recordPreloadCompletion(ctx context.Context, actorID, repositoryPK, jobID, repoType, repoID, status, commit string, total, processed, failed int, message string) {
	s.audit(ctx, actorID, "repository.preload.finished", "repository", repositoryPK, map[string]any{
		"job_id": jobID, "repo_type": repoType, "repo_id": repoID,
		"commit_sha": commit,
		"status":     status, "files_total": total, "files_processed": processed,
		"failed_files": failed, "error": message,
	})
}

func (s *Server) serveOrAcknowledgePreload(w http.ResponseWriter, r *http.Request, artifact artifact, cacheStatus string) {
	if _, ok := r.Context().Value(preloadFetchContextKey{}).(bool); ok {
		w.Header().Set("X-Cache", cacheStatus)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.serveArtifact(w, r, artifact, cacheStatus)
}

func (s *Server) listHubFiles(ctx context.Context, repoType, repoID, revision string) ([]string, string, error) {
	files := make([]string, 0)
	resolvedCommit := ""
	query := url.Values{"recursive": {"true"}, "expand": {"false"}, "limit": {fmt.Sprint(preloadPageSize)}}
	for page := 0; page < preloadMaxPages; page++ {
		requestPath := "/api/" + repoType + "/" + escapeRepoPath(repoID) + "/tree/" + url.PathEscape(revision)
		response, firstHeaders, err := s.fetchHub(ctx, http.MethodGet, requestPath, query, nil)
		if err != nil {
			return nil, "", fmt.Errorf("unable to list repository files from Hugging Face")
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			response.Body.Close()
			return nil, "", fmt.Errorf("Hugging Face returned HTTP %d while listing repository files", response.StatusCode)
		}
		if resolvedCommit == "" {
			resolvedCommit = firstHeaders.Get("X-Repo-Commit")
			if resolvedCommit == "" {
				resolvedCommit = response.Header.Get("X-Repo-Commit")
			}
			if !validCommit(resolvedCommit) {
				resolvedCommit = ""
			}
		}
		link := response.Header.Get("Link")
		data, readErr := io.ReadAll(io.LimitReader(response.Body, preloadPageLimit+1))
		response.Body.Close()
		if readErr != nil || len(data) > preloadPageLimit {
			return nil, "", fmt.Errorf("repository file listing exceeded the %d MiB page limit", preloadPageLimit>>20)
		}
		var entries []hubTreeEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, "", fmt.Errorf("Hugging Face returned an invalid repository file listing")
		}
		for _, entry := range entries {
			if entry.Type != "file" || entry.Path == "" {
				continue
			}
			files = append(files, entry.Path)
			if len(files) > preloadMaxFiles {
				return nil, "", fmt.Errorf("repository has more than %d files; preloading is limited to %d files per run", preloadMaxFiles, preloadMaxFiles)
			}
		}
		cursor := nextHubCursor(link)
		if cursor == "" {
			return files, resolvedCommit, nil
		}
		query.Set("cursor", cursor)
	}
	return nil, "", fmt.Errorf("repository file listing exceeded %d pages", preloadMaxPages)
}

func nextHubCursor(linkHeader string) string {
	for _, link := range strings.Split(linkHeader, ",") {
		if !strings.Contains(link, `rel="next"`) && !strings.Contains(link, "rel=next") {
			continue
		}
		start, end := strings.Index(link, "<"), strings.Index(link, ">")
		if start < 0 || end <= start+1 {
			continue
		}
		next, err := url.Parse(link[start+1 : end])
		if err != nil {
			continue
		}
		cursor := next.Query().Get("cursor")
		if len(cursor) <= 512 {
			return cursor
		}
	}
	return ""
}

func (s *Server) updatePreload(id string, update func(*preloadJob)) {
	s.preloadsMu.Lock()
	defer s.preloadsMu.Unlock()
	if job := s.preloads[id]; job != nil {
		update(job)
		job.UpdatedAt = time.Now().UTC()
	}
}

func (s *Server) preloadSnapshots(ctx context.Context, principal *Principal) ([]preloadJob, error) {
	s.preloadsMu.RLock()
	items := make([]preloadJob, 0, len(s.preloads))
	known := make(map[string]bool, len(s.preloads))
	fallback := make(map[string]preloadJob, len(s.preloads))
	for _, job := range s.preloads {
		key := job.RepoType + "/" + job.RepoID
		if job.Status == "queued" || job.Status == "running" {
			items = append(items, *job)
			known[key] = true
		} else {
			fallback[key] = *job
		}
	}
	s.preloadsMu.RUnlock()
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT r.id,r.repo_type,r.repo_id,e.action,e.metadata_json::text,e.created_at,
		CASE WHEN e.action='repository.preload.finished' AND e.metadata_json->>'status'='completed'
			THEN (SELECT COUNT(*) FROM artifacts a WHERE a.repository_id=r.id
				AND (COALESCE(e.metadata_json->>'commit_sha','')='' OR a.commit_sha=e.metadata_json->>'commit_sha'))
			ELSE -1 END AS cached_files
		FROM repositories r JOIN LATERAL (
			SELECT action,metadata_json,created_at FROM audit_events
			WHERE target_type='repository' AND target_id=r.id AND action IN ('repository.preload.started','repository.preload.finished')
			ORDER BY created_at DESC,id DESC LIMIT 1
		) e ON TRUE
		WHERE r.deleted_at IS NULL ORDER BY e.created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var repositoryPK, repoType, repoID, action, encodedMetadata string
		var updatedAt time.Time
		var cachedFiles int64
		if err := rows.Scan(&repositoryPK, &repoType, &repoID, &action, &encodedMetadata, &updatedAt, &cachedFiles); err != nil {
			return nil, err
		}
		key := repoType + "/" + repoID
		if known[key] {
			continue
		}
		var metadata struct {
			JobID          string `json:"job_id"`
			Status         string `json:"status"`
			CommitSHA      string `json:"commit_sha"`
			FilesTotal     int    `json:"files_total"`
			FilesProcessed int    `json:"files_processed"`
			FailedFiles    int    `json:"failed_files"`
			Error          string `json:"error"`
		}
		if err := json.Unmarshal([]byte(encodedMetadata), &metadata); err != nil {
			continue
		}
		status := metadata.Status
		if action == "repository.preload.started" {
			status = "interrupted"
			metadata.Error = "The server restarted before preloading finished. Retry the preload."
		} else if status == "completed" && cachedFiles < int64(metadata.FilesTotal) {
			status = "incomplete"
			metadata.Error = "Some cached files were purged. Preload the repository again to restore the complete cache."
		}
		items = append(items, preloadJob{
			ID: metadata.JobID, RepoType: repoType, RepoID: repoID, CommitSHA: metadata.CommitSHA, Status: status,
			FilesTotal: metadata.FilesTotal, FilesProcessed: metadata.FilesProcessed,
			FailedFiles: metadata.FailedFiles, Error: metadata.Error,
			StartedAt: updatedAt, UpdatedAt: updatedAt,
		})
		known[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for key, job := range fallback {
		if known[key] {
			continue
		}
		if job.Status == "completed" {
			var cachedFiles int64
			if err := s.db.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE r.repo_type=$1 AND r.repo_id=$2 AND a.commit_sha=$3`, job.RepoType, job.RepoID, job.CommitSHA).Scan(&cachedFiles); err != nil {
				return nil, err
			}
			if cachedFiles < int64(job.FilesTotal) {
				job.Status = "incomplete"
				job.Error = "Some cached files were purged. Preload the repository again to restore the complete cache."
			}
		}
		items = append(items, job)
	}
	if principal.Role != "admin" {
		visible := items[:0]
		for _, job := range items {
			if _, _, err := s.repositoryAccess(ctx, principal, nil, job.RepoType, job.RepoID); err == nil {
				visible = append(visible, job)
			}
		}
		items = visible
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.After(items[j].StartedAt) })
	if len(items) > 200 {
		items = items[:200]
	}
	return items, nil
}
