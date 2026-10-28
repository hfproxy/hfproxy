package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/storage"
)

const (
	downloadResumeChunkSize = int64(64 << 20)
	downloadResumeMaxAge    = 7 * 24 * time.Hour
)

type downloadResume struct {
	ID             string
	RepositoryID   string
	CommitSHA      string
	Path           string
	ETag           string
	TotalSize      int64
	ContentType    string
	ChunkSize      int64
	CompletedBytes int64
}

func (s *Server) lockDownloadResume(ctx context.Context, repositoryID, commit, filePath string) (func(), error) {
	conn, err := s.db.SQL.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire download resume connection: %w", err)
	}
	lockKey := repositoryID + "|" + commit + "|" + filePath
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("lock download resume: %w", err)
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey)
		_ = conn.Close() // Closing the session also releases the lock if unlock failed.
	}, nil
}

type upstreamStatusError struct {
	status int
	header http.Header
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("Hugging Face returned HTTP %d while downloading a file", e.status)
}

func (s *Server) downloadResumable(ctx context.Context, objectStore *storage.S3, downloadID, repositoryID, commit string, parsed resolvePath, total int64, etag, contentType string, stream io.Writer) (*os.File, string, *downloadResume, error) {
	resume, err := s.loadDownloadResume(ctx, objectStore, repositoryID, commit, parsed.FilePath, total, etag, contentType)
	if err != nil {
		return nil, "", nil, err
	}
	_, _ = s.db.SQL.ExecContext(ctx, `UPDATE downloads SET bytes=$2 WHERE id=$1 AND status='downloading'`, downloadID, resume.CompletedBytes)
	if stream != nil && resume.CompletedBytes > 0 {
		if err := s.streamCompletedResume(ctx, objectStore, resume, stream); err != nil {
			return nil, "", resume, err
		}
	}

	for resume.CompletedBytes < resume.TotalSize {
		start := resume.CompletedBytes
		end := start + resume.ChunkSize - 1
		if end >= resume.TotalSize {
			end = resume.TotalSize - 1
		}
		headers := make(http.Header)
		headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		if resume.ETag != "" {
			headers.Set("If-Range", resume.ETag)
		}
		pinned := resolvePath{RepoType: parsed.RepoType, RepoID: parsed.RepoID, Revision: commit, FilePath: parsed.FilePath}
		response, firstHeaders, fetchErr := s.fetchHub(ctx, http.MethodGet, upstreamResolvePath(pinned), nil, headers)
		if fetchErr != nil {
			return nil, "", resume, fetchErr
		}
		responseETag := response.Header.Get("ETag")
		if responseETag == "" {
			responseETag = firstHeaders.Get("ETag")
		}

		if response.StatusCode == http.StatusOK {
			if start > 0 || (resume.ETag != "" && responseETag != "" && resume.ETag != responseETag) {
				if err := s.resetDownloadResume(ctx, objectStore, resume, responseETag, resume.TotalSize, resume.ContentType); err != nil {
					response.Body.Close()
					return nil, "", resume, err
				}
			}
			if response.ContentLength >= 0 && response.ContentLength != resume.TotalSize {
				response.Body.Close()
				return nil, "", resume, fmt.Errorf("Hub file size changed while resuming")
			}
			if responseETag != "" && resume.ETag != responseETag {
				if err := s.updateDownloadResumeETag(ctx, resume, responseETag); err != nil {
					response.Body.Close()
					return nil, "", resume, err
				}
			}
			err = s.saveFullDownloadResponse(ctx, objectStore, downloadID, resume, response.Body, stream)
			response.Body.Close()
			if err != nil {
				return nil, "", resume, err
			}
			break
		}

		if response.StatusCode != http.StatusPartialContent {
			response.Body.Close()
			return nil, "", resume, &upstreamStatusError{status: response.StatusCode, header: response.Header.Clone()}
		}

		if resume.ETag != "" && responseETag != "" && resume.ETag != responseETag {
			response.Body.Close()
			if err := s.resetDownloadResume(ctx, objectStore, resume, responseETag, resume.TotalSize, resume.ContentType); err != nil {
				return nil, "", resume, err
			}
			continue
		}
		if !validContentRange(response.Header.Get("Content-Range"), start, end, resume.TotalSize) {
			response.Body.Close()
			return nil, "", resume, fmt.Errorf("Hub returned an invalid byte range while resuming")
		}
		if response.ContentLength >= 0 && response.ContentLength != end-start+1 {
			response.Body.Close()
			return nil, "", resume, fmt.Errorf("Hub returned an incomplete byte range while resuming")
		}
		if responseETag != "" && resume.ETag == "" {
			if err := s.updateDownloadResumeETag(ctx, resume, responseETag); err != nil {
				response.Body.Close()
				return nil, "", resume, err
			}
		}
		err = s.saveDownloadChunk(ctx, objectStore, downloadID, resume, response.Body, start, end-start+1, stream)
		response.Body.Close()
		if err != nil {
			return nil, "", resume, err
		}
	}

	file, digest, err := s.assembleDownloadResume(ctx, objectStore, resume)
	if err != nil {
		return nil, "", resume, err
	}
	return file, digest, resume, nil
}

func (s *Server) loadDownloadResume(ctx context.Context, objectStore *storage.S3, repositoryID, commit, filePath string, total int64, etag, contentType string) (*downloadResume, error) {
	resume := &downloadResume{}
	err := s.db.SQL.QueryRowContext(ctx, `SELECT id,repository_id,commit_sha,path,upstream_etag,total_size,content_type,chunk_size,completed_bytes
		FROM download_resumptions WHERE repository_id=$1 AND commit_sha=$2 AND path=$3`, repositoryID, commit, filePath).Scan(
		&resume.ID, &resume.RepositoryID, &resume.CommitSHA, &resume.Path, &resume.ETag, &resume.TotalSize, &resume.ContentType, &resume.ChunkSize, &resume.CompletedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		resume = &downloadResume{ID: db.NewID(), RepositoryID: repositoryID, CommitSHA: commit, Path: filePath, ETag: etag, TotalSize: total, ContentType: contentType, ChunkSize: downloadResumeChunkSize}
		_, err = s.db.SQL.ExecContext(ctx, `INSERT INTO download_resumptions(id,repository_id,commit_sha,path,upstream_etag,total_size,content_type,chunk_size,completed_bytes)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,0)`, resume.ID, repositoryID, commit, filePath, etag, total, contentType, downloadResumeChunkSize)
		if err != nil {
			return nil, fmt.Errorf("create resumable download state: %w", err)
		}
		return resume, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read resumable download state: %w", err)
	}

	invalidProgress := resume.CompletedBytes < 0 || resume.CompletedBytes > resume.TotalSize || resume.ChunkSize <= 0 || (resume.CompletedBytes != resume.TotalSize && resume.CompletedBytes%resume.ChunkSize != 0)
	changedFile := resume.TotalSize != total || (resume.ETag != "" && etag != "" && resume.ETag != etag)
	if invalidProgress || changedFile {
		if err := s.resetDownloadResume(ctx, objectStore, resume, etag, total, contentType); err != nil {
			return nil, err
		}
	}
	return resume, nil
}

func (s *Server) resetDownloadResume(ctx context.Context, objectStore *storage.S3, resume *downloadResume, etag string, total int64, contentType string) error {
	if err := deleteDownloadResumeParts(ctx, objectStore, resume); err != nil {
		return fmt.Errorf("discard stale partial download: %w", err)
	}
	if etag == "" {
		etag = resume.ETag
	}
	if contentType == "" {
		contentType = resume.ContentType
	}
	_, err := s.db.SQL.ExecContext(ctx, `UPDATE download_resumptions SET upstream_etag=$2,total_size=$3,content_type=$4,chunk_size=$5,completed_bytes=0,updated_at=now() WHERE id=$1`, resume.ID, etag, total, contentType, downloadResumeChunkSize)
	if err != nil {
		return fmt.Errorf("reset resumable download state: %w", err)
	}
	resume.ETag, resume.TotalSize, resume.ContentType = etag, total, contentType
	resume.ChunkSize, resume.CompletedBytes = downloadResumeChunkSize, 0
	return nil
}

func (s *Server) updateDownloadResumeETag(ctx context.Context, resume *downloadResume, etag string) error {
	if etag == "" || etag == resume.ETag {
		return nil
	}
	if _, err := s.db.SQL.ExecContext(ctx, `UPDATE download_resumptions SET upstream_etag=$2,updated_at=now() WHERE id=$1`, resume.ID, etag); err != nil {
		return fmt.Errorf("save resumable download validator: %w", err)
	}
	resume.ETag = etag
	return nil
}

func (s *Server) saveFullDownloadResponse(ctx context.Context, objectStore *storage.S3, downloadID string, resume *downloadResume, body io.Reader, stream io.Writer) error {
	for resume.CompletedBytes < resume.TotalSize {
		length := resume.ChunkSize
		if remaining := resume.TotalSize - resume.CompletedBytes; remaining < length {
			length = remaining
		}
		if err := s.saveDownloadChunk(ctx, objectStore, downloadID, resume, body, resume.CompletedBytes, length, stream); err != nil {
			return err
		}
	}
	var extra [1]byte
	n, err := io.ReadFull(body, extra[:])
	if n > 0 {
		if resetErr := s.resetDownloadResume(ctx, objectStore, resume, resume.ETag, resume.TotalSize, resume.ContentType); resetErr != nil {
			return resetErr
		}
		return fmt.Errorf("Hub response size did not match its metadata")
	}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("Hub response size did not match its metadata")
	}
	return nil
}

func (s *Server) saveDownloadChunk(ctx context.Context, objectStore *storage.S3, downloadID string, resume *downloadResume, body io.Reader, offset, length int64, stream io.Writer) error {
	tmp, err := os.CreateTemp("", "hfproxy-part-*")
	if err != nil {
		return fmt.Errorf("create resumable download block: %w", err)
	}
	defer func() {
		name := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	destination := io.Writer(tmp)
	if stream != nil {
		destination = io.MultiWriter(tmp, stream)
	}
	progress := &downloadProgressWriter{ctx: ctx, db: s.db.SQL, id: downloadID, baseBytes: offset, destination: destination, lastPersist: time.Now()}
	written, err := io.CopyN(progress, body, length)
	if err != nil || written != length {
		return fmt.Errorf("Hub transfer stopped before the current block completed")
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("finalize resumable download block: %w", err)
	}
	part := int(offset / resume.ChunkSize)
	if err := objectStore.UploadFile(ctx, tmp, objectStore.PartialKey(resume.ID, part), resume.ContentType, length); err != nil {
		return fmt.Errorf("persist resumable download block: %w", err)
	}
	result, err := s.db.SQL.ExecContext(ctx, `UPDATE download_resumptions SET completed_bytes=$2,updated_at=now() WHERE id=$1 AND completed_bytes=$3`, resume.ID, offset+length, offset)
	if err != nil {
		return fmt.Errorf("save resumable download progress: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("resumable download progress changed unexpectedly")
	}
	resume.CompletedBytes = offset + length
	return nil
}

func (s *Server) streamCompletedResume(ctx context.Context, objectStore *storage.S3, resume *downloadResume, stream io.Writer) error {
	for offset := int64(0); offset < resume.CompletedBytes; offset += resume.ChunkSize {
		length := resume.ChunkSize
		if remaining := resume.CompletedBytes - offset; remaining < length {
			length = remaining
		}
		out, err := objectStore.Get(ctx, objectStore.PartialKey(resume.ID, int(offset/resume.ChunkSize)), "")
		if err != nil {
			return fmt.Errorf("read a persisted download block: %w", err)
		}
		written, copyErr := io.CopyN(stream, out.Body, length)
		closeErr := out.Body.Close()
		if copyErr != nil || closeErr != nil || written != length {
			return fmt.Errorf("stream a persisted download block")
		}
	}
	return nil
}

func (s *Server) assembleDownloadResume(ctx context.Context, objectStore *storage.S3, resume *downloadResume) (*os.File, string, error) {
	tmp, err := os.CreateTemp("", "hfproxy-staging-*")
	if err != nil {
		return nil, "", fmt.Errorf("create staged download: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	hash := sha256.New()
	var assembled int64
	for part := 0; assembled < resume.TotalSize; part++ {
		out, err := objectStore.Get(ctx, objectStore.PartialKey(resume.ID, part), "")
		if err != nil {
			if ctx.Err() == nil {
				_ = s.resetDownloadResume(ctx, objectStore, resume, resume.ETag, resume.TotalSize, resume.ContentType)
			}
			return nil, "", fmt.Errorf("read a persisted download block: %w", err)
		}
		length := resume.ChunkSize
		if remaining := resume.TotalSize - assembled; remaining < length {
			length = remaining
		}
		written, copyErr := io.CopyN(io.MultiWriter(tmp, hash), out.Body, length)
		closeErr := out.Body.Close()
		if copyErr != nil || closeErr != nil || written != length {
			if ctx.Err() == nil {
				_ = s.resetDownloadResume(ctx, objectStore, resume, resume.ETag, resume.TotalSize, resume.ContentType)
			}
			return nil, "", fmt.Errorf("a persisted download block was incomplete")
		}
		assembled += written
	}
	if assembled != resume.TotalSize {
		return nil, "", fmt.Errorf("assembled download size did not match Hugging Face metadata")
	}
	if err := tmp.Sync(); err != nil {
		return nil, "", fmt.Errorf("finalize staged download: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("rewind staged download: %w", err)
	}
	keep = true
	return tmp, hex.EncodeToString(hash.Sum(nil)), nil
}

func validContentRange(value string, start, end, total int64) bool {
	unit, rangeValue, ok := strings.Cut(value, " ")
	if !ok || unit != "bytes" {
		return false
	}
	positions, totalValue, ok := strings.Cut(rangeValue, "/")
	if !ok {
		return false
	}
	startValue, endValue, ok := strings.Cut(positions, "-")
	if !ok {
		return false
	}
	actualStart, err1 := strconv.ParseInt(startValue, 10, 64)
	actualEnd, err2 := strconv.ParseInt(endValue, 10, 64)
	actualTotal, err3 := strconv.ParseInt(totalValue, 10, 64)
	return err1 == nil && err2 == nil && err3 == nil && actualStart == start && actualEnd == end && actualTotal == total
}

func deleteDownloadResumeParts(ctx context.Context, objectStore *storage.S3, resume *downloadResume) error {
	if resume.ChunkSize <= 0 {
		return nil
	}
	parts := int((resume.CompletedBytes + resume.ChunkSize - 1) / resume.ChunkSize)
	if parts == 0 {
		parts = 1
	}
	parts++ // Include a block uploaded just before a process stopped.
	maxParts := int((resume.TotalSize+resume.ChunkSize-1)/resume.ChunkSize) + 1
	if parts > maxParts {
		parts = maxParts
	}
	for part := 0; part < parts; part++ {
		if err := objectStore.Delete(ctx, objectStore.PartialKey(resume.ID, part)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) finishDownloadResume(ctx context.Context, objectStore *storage.S3, resume *downloadResume) {
	if err := deleteDownloadResumeParts(ctx, objectStore, resume); err != nil {
		s.logger.Warn("could not remove completed download blocks", "error", err)
		return
	}
	if _, err := s.db.SQL.ExecContext(ctx, `DELETE FROM download_resumptions WHERE id=$1`, resume.ID); err != nil {
		s.logger.Warn("could not remove completed download state", "error", err)
	}
}

func (s *Server) startDownloadResumeCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		s.cleanupStaleDownloadResumptions(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.cleanupStaleDownloadResumptions(ctx)
			}
		}
	}()
}

func (s *Server) cleanupStaleDownloadResumptions(ctx context.Context) {
	maxAgeHours := float64(downloadResumeMaxAge / time.Hour)
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT id,repository_id,commit_sha,path,upstream_etag,total_size,content_type,chunk_size,completed_bytes
		FROM download_resumptions WHERE updated_at < now()-($1::double precision * interval '1 hour') ORDER BY updated_at LIMIT 100`, maxAgeHours)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("could not list stale download blocks", "error", err)
		}
		return
	}
	var stale []*downloadResume
	for rows.Next() {
		item := &downloadResume{}
		if err := rows.Scan(&item.ID, &item.RepositoryID, &item.CommitSHA, &item.Path, &item.ETag, &item.TotalSize, &item.ContentType, &item.ChunkSize, &item.CompletedBytes); err != nil {
			rows.Close()
			s.logger.Warn("could not read stale download state", "error", err)
			return
		}
		stale = append(stale, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.logger.Warn("could not finish reading stale download state", "error", err)
		return
	}
	rows.Close()

	for _, item := range stale {
		if ctx.Err() != nil {
			return
		}
		s.cleanupStaleDownloadResume(ctx, item)
	}
}

func (s *Server) cleanupStaleDownloadResume(ctx context.Context, candidate *downloadResume) {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	lockKey := candidate.RepositoryID + ":" + candidate.CommitSHA + ":" + candidate.Path
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`, lockKey).Scan(&locked); err != nil || !locked {
		return
	}
	current := &downloadResume{}
	maxAgeHours := float64(downloadResumeMaxAge / time.Hour)
	err = tx.QueryRowContext(ctx, `SELECT id,repository_id,commit_sha,path,upstream_etag,total_size,content_type,chunk_size,completed_bytes
		FROM download_resumptions WHERE id=$1 AND updated_at < now()-($2::double precision * interval '1 hour')`, candidate.ID, maxAgeHours).Scan(
		&current.ID, &current.RepositoryID, &current.CommitSHA, &current.Path, &current.ETag, &current.TotalSize, &current.ContentType, &current.ChunkSize, &current.CompletedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		s.logger.Warn("could not recheck stale download state", "error", err)
		return
	}
	if err := deleteDownloadResumeParts(ctx, s.currentStorage(), current); err != nil {
		s.logger.Warn("could not remove stale download blocks", "error", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM download_resumptions WHERE id=$1`, current.ID); err != nil {
		s.logger.Warn("could not remove stale download state", "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Warn("could not commit stale download cleanup", "error", err)
	}
}
