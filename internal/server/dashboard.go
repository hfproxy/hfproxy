package server

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/db"
)

type dashboardRepo struct {
	Type       string
	ID         string
	Policy     string
	Artifacts  int64
	LastActive string
}

type dashboardArtifact struct {
	RepoType string
	RepoID   string
	Path     string
	Commit   string
	Size     int64
	CachedAt string
}

type dashboardData struct {
	Principal     *Principal
	CSRF          string
	Logout        template.HTML
	IsAdmin       bool
	RepoCount     int64
	ArtifactCount int64
	CacheBytes    int64
	CacheSize     string
	HitRatio      string
	Repos         []dashboardRepo
	Artifacts     []dashboardArtifact
	PublicRead    bool
	HFBase        string
	PublicURL     string
	OIDCEnabled   bool
	AuthMode      string
	Message       string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		if p.Role != "admin" || !s.checkCSRF(r) {
			writeProblem(w, r, http.StatusForbidden, "Forbidden", "An administrator session and a valid CSRF token are required.")
			return
		}
		if err := r.ParseForm(); err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid form", "Unable to parse the submitted form.")
			return
		}
		repoType := r.FormValue("repo_type")
		repoID := strings.TrimSpace(r.FormValue("repo_id"))
		policy := r.FormValue("access_policy")
		if !validRepoType(repoType) || !validRepoID(repoID) || (policy != "public" && policy != "allowlist" && policy != "admin_only") {
			s.renderDashboard(w, r, p, "Vérifiez le type, l’identifiant et la politique du dépôt.", http.StatusBadRequest)
			return
		}
		id := ""
		err := s.db.SQL.QueryRowContext(r.Context(), `INSERT INTO repositories(id,repo_type,repo_id,access_policy,created_by)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT(repo_type,repo_id) DO UPDATE SET access_policy=EXCLUDED.access_policy,deleted_at=NULL,updated_at=now()
			RETURNING id`, db.NewID(), repoType, repoID, policy, p.ID).Scan(&id)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Repository error", "Unable to save repository policy.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.upsert", "repository", id, map[string]any{"repo_type": repoType, "repo_id": repoID, "access_policy": policy})
		http.Redirect(w, r, "/?saved=1", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	message := ""
	if r.URL.Query().Get("saved") == "1" {
		message = "La politique du dépôt a été enregistrée."
	}
	s.renderDashboard(w, r, p, message, http.StatusOK)
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, p *Principal, message string, status int) {
	data := dashboardData{Principal: p, IsAdmin: p.Role == "admin", PublicRead: s.currentConfig().AnonymousPublicRead,
		HFBase: s.currentConfig().HFBaseURL, PublicURL: s.currentConfig().PublicURL, OIDCEnabled: s.currentOIDCConfig() != nil, AuthMode: s.currentConfig().AuthMode, Message: message}
	if csrf, err := r.Cookie("hfproxy_csrf"); err == nil {
		data.CSRF = csrf.Value
		data.Logout = s.renderDashboardLogout(w, csrf.Value)
	}
	visibility := `($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))`
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM repositories r WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&data.RepoCount)
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE `+visibility, p.Role, p.ID).Scan(&data.ArtifactCount, &data.CacheBytes)
	data.CacheSize = formatBytes(data.CacheBytes)
	var hits, misses int64
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FILTER(WHERE d.status='hit'),COUNT(*) FILTER(WHERE d.status='miss') FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE `+visibility, p.Role, p.ID).Scan(&hits, &misses)
	if hits+misses > 0 {
		data.HitRatio = formatRatio(float64(hits) / float64(hits+misses))
	} else {
		data.HitRatio = "—"
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT r.repo_type,r.repo_id,r.access_policy,COUNT(a.id),COALESCE(MAX(a.last_accessed_at),r.updated_at)
		FROM repositories r LEFT JOIN artifacts a ON a.repository_id=r.id WHERE r.deleted_at IS NULL AND `+visibility+`
		GROUP BY r.id ORDER BY COALESCE(MAX(a.last_accessed_at),r.updated_at) DESC LIMIT 8`, p.Role, p.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var item dashboardRepo
			var active time.Time
			if rows.Scan(&item.Type, &item.ID, &item.Policy, &item.Artifacts, &active) == nil {
				item.LastActive = active.Local().Format("02 Jan · 15:04")
				data.Repos = append(data.Repos, item)
			}
		}
	}
	artifactRows, err := s.db.SQL.QueryContext(r.Context(), `SELECT r.repo_type,r.repo_id,a.path,a.commit_sha,b.size_bytes,a.cached_at
		FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE `+visibility+`
		ORDER BY a.cached_at DESC LIMIT 8`, p.Role, p.ID)
	if err == nil {
		defer artifactRows.Close()
		for artifactRows.Next() {
			var item dashboardArtifact
			var cached time.Time
			if artifactRows.Scan(&item.RepoType, &item.RepoID, &item.Path, &item.Commit, &item.Size, &cached) == nil {
				item.Commit = shortCommit(item.Commit)
				item.CachedAt = cached.Local().Format("02 Jan · 15:04")
				data.Artifacts = append(data.Artifacts, item)
			}
		}
	}
	tmpl, err := template.New("dashboard.html").Funcs(template.FuncMap{
		"avatar": func(value string) string {
			value = strings.TrimSpace(value)
			if value == "" {
				return "H"
			}
			for _, r := range value {
				return strings.ToUpper(string(r))
			}
			return "H"
		},
		"repoIcon": func(repoType string) string {
			if repoType == "datasets" {
				return "▥"
			}
			if repoType == "spaces" {
				return "◇"
			}
			return "◈"
		},
		"policyLabel": func(policy string) string {
			switch policy {
			case "allowlist":
				return "Liste d’accès"
			case "admin_only":
				return "Admins"
			default:
				return "Public"
			}
		},
		"bytes": formatBytes,
	}).ParseFS(dashboardFS, "dashboard.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tmpl.Execute(w, data)
}

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
