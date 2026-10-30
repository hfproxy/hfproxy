package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/hfproxy/hfproxy/internal/db"
)

// unsafeExtensions are the file types that can run code when a client loads
// them: pickles and the formats built on them (PyTorch checkpoints, joblib
// dumps, Paddle parameters). Legacy ggml ".bin" files are caught too, since the
// name does not tell them from a PyTorch ".bin"; a repository that needs one
// can be exempted. .npy and .npz stay allowed: NumPy does not unpickle them
// unless the caller asks.
var unsafeExtensions = map[string]bool{
	".bin": true, ".pt": true, ".pth": true, ".ckpt": true,
	".pkl": true, ".pickle": true, ".pck": true, ".joblib": true,
	".pdparams": true,
}

// unsafeFormat reports whether a file's name marks it as a format that can run
// code when loaded.
func unsafeFormat(filePath string) bool {
	return unsafeExtensions[strings.ToLower(path.Ext(filePath))]
}

// formatRefusal returns why a file is refused under the safe-formats setting,
// or "" when it may be served or published. allow is the repository's
// exemption.
func (s *Server) formatRefusal(allow bool, filePath string) string {
	if allow || !s.currentConfig().SafeFormatsOnly || !unsafeFormat(filePath) {
		return ""
	}
	return fmt.Sprintf("%s is not available: this proxy only serves formats that cannot run code when they are loaded. Use safetensors, or ask an administrator to allow this repository.", path.Base(filePath))
}

func (s *Server) countRefusedFormat(where string) {
	if s.metrics != nil {
		s.metrics.add("hfproxy_unsafe_format_refused_total", []string{"where=" + where}, 1)
	}
}

// maxSafetensorsHeader is the largest header the safetensors library accepts.
const maxSafetensorsHeader = 100 << 20

// checkSafetensors verifies that a file named .safetensors is one: an 8-byte
// little-endian header length, then that many bytes of a JSON object, inside
// the file. A pickle renamed to .safetensors fails it. readAt returns n bytes
// from an offset.
func checkSafetensors(size int64, readAt func(offset, n int64) ([]byte, error)) error {
	if size < 8 {
		return fmt.Errorf("it is only %d bytes long", size)
	}
	prefix, err := readAt(0, 8)
	if err != nil || len(prefix) != 8 {
		return fmt.Errorf("its header could not be read")
	}
	n := binary.LittleEndian.Uint64(prefix)
	if n == 0 || n > maxSafetensorsHeader || int64(n) > size-8 {
		return fmt.Errorf("its header length (%d) does not fit the file", n)
	}
	header, err := readAt(8, int64(n))
	if err != nil || int64(len(header)) != int64(n) {
		return fmt.Errorf("its header could not be read")
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(header, &parsed); err != nil {
		return fmt.Errorf("its header is not a JSON object")
	}
	return nil
}

// checkStoredSafetensors reads the header of a stored blob and verifies it.
func (s *Server) checkStoredSafetensors(ctx context.Context, sha string, size int64) error {
	objects := s.currentStorage()
	if objects == nil {
		return fmt.Errorf("object storage is not configured")
	}
	key := objects.Key(sha)
	return checkSafetensors(size, func(offset, n int64) ([]byte, error) {
		out, err := objects.Get(ctx, key, fmt.Sprintf("bytes=%d-%d", offset, offset+n-1))
		if err != nil {
			return nil, err
		}
		defer out.Body.Close()
		return io.ReadAll(io.LimitReader(out.Body, n))
	})
}

// checkFormats applies the safe-formats setting to what a commit adds: a file
// that can run code is refused, and a file named .safetensors must be one.
func (s *Server) checkFormats(ctx context.Context, repoPK string, ops []db.CommitOp) *commitFailure {
	if !s.currentConfig().SafeFormatsOnly {
		return nil
	}
	repo, err := s.db.GetRepository(ctx, repoPK)
	if err != nil {
		return &commitFailure{http.StatusInternalServerError, "", "The repository could not be read."}
	}
	if repo.AllowUnsafeFormats {
		return nil
	}
	for _, op := range ops {
		if op.Kind != db.OpAdd {
			continue
		}
		if refusal := s.formatRefusal(false, op.Path); refusal != "" {
			s.countRefusedFormat("publish")
			return &commitFailure{http.StatusUnprocessableEntity, "", refusal}
		}
		if strings.EqualFold(path.Ext(op.Path), ".safetensors") && op.SHA256 != "" {
			if err := s.checkStoredSafetensors(ctx, op.SHA256, op.Size); err != nil {
				s.countRefusedFormat("publish")
				return &commitFailure{http.StatusUnprocessableEntity, "", fmt.Sprintf("%s is not a valid safetensors file: %v.", path.Base(op.Path), err)}
			}
		}
	}
	return nil
}
