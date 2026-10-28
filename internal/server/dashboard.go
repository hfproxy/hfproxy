package server

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) css(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	content, err := dashboardFS.ReadFile("app.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if r.Method == http.MethodGet {
		_, _ = w.Write(content)
	}
}

func validRepoType(repoType string) bool {
	return repoType == "models" || repoType == "datasets" || repoType == "spaces"
}

func validRepoID(repoID string) bool {
	if repoID == "" || len(repoID) > 256 || strings.HasPrefix(repoID, "/") || strings.HasSuffix(repoID, "/") || strings.Contains(repoID, "\\") {
		return false
	}
	parts := strings.Split(repoID, "/")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
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

func formatBytes(n int64) string {
	if n < 1024 {
		return formatInt(n) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == "TiB" {
			return formatFloat(value) + " " + unit
		}
	}
	return "0 B"
}

func formatInt(v int64) string     { return strconv.FormatInt(v, 10) }
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
func formatRatio(v float64) string { return strconv.FormatFloat(v*100, 'f', 1, 64) + "%" }
func shortCommit(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}
