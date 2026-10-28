package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/storage"
	"golang.org/x/oauth2"
)

//go:embed dashboard.html
var dashboardFS embed.FS

//go:embed all:web
var frontendFS embed.FS

type runtimeState struct {
	config       config.Config
	storage      *storage.S3
	httpClient   *http.Client
	oidcProvider *oidc.Provider
	oidcConfig   *oauth2.Config
	oidcVerifier *oidc.IDTokenVerifier
}

type Server struct {
	runtime        atomic.Pointer[runtimeState]
	db             *db.Store
	logger         *slog.Logger
	preloadsMu     sync.RWMutex
	preloads       map[string]*preloadJob
	preloadSlots   chan struct{}
	loginMu        sync.Mutex
	loginBuckets   map[string]loginBucket
	loginHashSlots chan struct{}
	negativeMu     sync.Mutex
	negative       map[string]negativeCacheEntry
	missMu         sync.Mutex
	misses         map[string]*cacheMiss
}

type loginBucket struct {
	window time.Time
	count  int
}

type negativeCacheEntry struct {
	expires time.Time
	status  int
	headers http.Header
}

type cacheMiss struct{ done chan struct{} }

type Principal struct {
	ID            string   `json:"id"`
	Username      string   `json:"username"`
	Email         string   `json:"email,omitempty"`
	DisplayName   string   `json:"display_name,omitempty"`
	Role          string   `json:"role"`
	Authenticated bool     `json:"authenticated"`
	TokenID       string   `json:"-"`
	Scopes        []string `json:"-"`
}

// Version is embedded in upstream User-Agent headers and set by release builds.
var Version = "0.0.3"

func New(ctx context.Context, cfg config.Config, store *db.Store, objectStore *storage.S3, logger *slog.Logger) (*Server, error) {
	state, err := makeRuntimeState(ctx, cfg, objectStore, nil)
	if err != nil {
		return nil, err
	}
	s := &Server{
		db: store, logger: logger, preloads: make(map[string]*preloadJob),
		preloadSlots: make(chan struct{}, 2), loginBuckets: make(map[string]loginBucket), loginHashSlots: make(chan struct{}, 4), negative: make(map[string]negativeCacheEntry), misses: make(map[string]*cacheMiss),
	}
	s.runtime.Store(state)
	s.startDownloadResumeCleanup(ctx)
	s.startOrphanedBlobGC(ctx)
	return s, nil
}

func makeRuntimeState(ctx context.Context, cfg config.Config, objectStore *storage.S3, previous *runtimeState) (*runtimeState, error) {
	state := &runtimeState{config: cfg, storage: objectStore, httpClient: newHFHTTPClient(cfg)}
	if cfg.OIDCIssuer != "" {
		var provider *oidc.Provider
		if previous != nil && previous.config.OIDCIssuer == cfg.OIDCIssuer {
			provider = previous.oidcProvider
		} else {
			var err error
			provider, err = oidc.NewProvider(ctx, cfg.OIDCIssuer)
			if err != nil {
				return nil, fmt.Errorf("discover OIDC provider: %w", err)
			}
		}
		state.oidcProvider = provider
		state.oidcConfig = &oauth2.Config{
			ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.OIDCRedirectURL,
			Endpoint:    provider.Endpoint(),
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
		}
		state.oidcVerifier = provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID})
	}
	return state, nil
}

func (s *Server) currentConfig() config.Config               { return s.runtime.Load().config }
func (s *Server) currentStorage() *storage.S3                { return s.runtime.Load().storage }
func (s *Server) currentHTTPClient() *http.Client            { return s.runtime.Load().httpClient }
func (s *Server) currentOIDCConfig() *oauth2.Config          { return s.runtime.Load().oidcConfig }
func (s *Server) currentOIDCVerifier() *oidc.IDTokenVerifier { return s.runtime.Load().oidcVerifier }

func (s *Server) applyConfig(ctx context.Context, cfg config.Config, actorID string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	objects, err := storage.New(ctx, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption)
	if err != nil {
		return fmt.Errorf("prepare S3 settings")
	}
	state, err := makeRuntimeState(ctx, cfg, objects, s.runtime.Load())
	if err != nil {
		return err
	}
	if err := config.SavePersisted(ctx, s.db.SQL, cfg, actorID); err != nil {
		return err
	}
	s.runtime.Store(state)
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/assets/app.css", s.css)
	mux.HandleFunc("/login", s.login)
	mux.HandleFunc("/logout", s.logout)
	mux.HandleFunc("/auth/login", s.oidcLogin)
	mux.HandleFunc("/auth/callback", s.oidcCallback)
	mux.HandleFunc("/api/v1/auth/login", s.login)
	mux.HandleFunc("/api/v1/auth/callback", s.oidcCallback)
	mux.HandleFunc("/api/v1/auth/logout", s.logout)
	mux.HandleFunc("/api/v1/", s.apiV1)
	mux.HandleFunc("/api/", s.hubAPI)
	mux.HandleFunc("/", s.dashboardOrResolve)
	return s.securityHeaders(s.requestLog(mux))
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	server := &http.Server{
		Addr: s.currentConfig().ListenAddr, Handler: s.Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	s.logger.Info("http server started", "address", s.currentConfig().ListenAddr)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return <-errCh
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.WriteString(w, `{"status":"ok"}`)
	}
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	version, migrationErr := db.MigrationStatus(ctx, s.db.SQL)
	if s.db.SQL.PingContext(ctx) != nil || migrationErr != nil || version < 4 || s.currentStorage().Ready(ctx) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) dashboardOrResolve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.frontend(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/models/") || strings.HasPrefix(r.URL.Path, "/datasets/") || strings.HasPrefix(r.URL.Path, "/spaces/") {
		s.resolveFile(w, r)
		return
	}
	// huggingface_hub omits the repository type for model downloads, using
	// /{repo_id}/resolve/{revision}/{filename}. Treat that standard form as
	// the models equivalent while keeping the SPA fallback for other paths.
	if _, err := parseResolvePath("/models" + r.URL.EscapedPath()); err == nil {
		s.resolveFile(w, r)
		return
	}
	s.frontend(w, r)
}

func (s *Server) frontend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		name = "index.html"
	}
	asset := "web/dist/" + name
	content, err := frontendFS.ReadFile(asset)
	if err != nil {
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		content, err = frontendFS.ReadFile("web/dist/index.html")
		name = "index.html"
	}
	if err != nil {
		content, err = frontendFS.ReadFile("web/index.html")
		name = "index.html"
	}
	if err != nil {
		http.Error(w, "admin interface is not built", http.StatusServiceUnavailable)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if name == "index.html" {
		contentType = "text/html; charset=utf-8"
		w.Header().Set("Cache-Control", "no-cache")
	} else if immutableAsset(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(content)
	}
}

func immutableAsset(name string) bool {
	base := path.Base(name)
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 {
		return false
	}
	dash := strings.LastIndexByte(base[:dot], '-')
	if dash < 0 || dot-dash-1 < 8 {
		return false
	}
	for _, r := range base[dash+1 : dot] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseRecorder) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *responseRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-ID")
		if len(rid) > 100 || strings.ContainsAny(rid, "\r\n") {
			rid = ""
		}
		if rid == "" {
			rid = randomID()
		}
		w.Header().Set("X-Request-ID", rid)
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid)))
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.logger.Info("http request", "request_id", rid, "method", r.Method, "route", safeRoute(r.URL.Path), "status", status, "duration_ms", time.Since(start).Milliseconds(), "bytes", recorder.bytes)
	})
}

type requestIDKey struct{}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func safeRoute(path string) string {
	if len(path) > 240 {
		return path[:240]
	}
	return path
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": title, "status": status,
		"detail": detail, "request_id": requestID(r.Context()),
	})
}

func methodNotAllowed(w http.ResponseWriter) {
	methodNotAllowedWith(w, "GET, HEAD, POST, PATCH, DELETE")
}

func methodNotAllowedWith(w http.ResponseWriter, methods string) {
	w.Header().Set("Allow", methods)
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": "Method not allowed", "status": http.StatusMethodNotAllowed,
	})
}

func (s *Server) audit(ctx context.Context, actorID, action, targetType, targetID string, metadata map[string]any) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > 4096 {
		encoded = []byte(`{}`)
	}
	_, err = s.db.SQL.ExecContext(ctx, `INSERT INTO audit_events(id, actor_user_id, action, target_type, target_id, metadata_json, request_id)
		VALUES ($1, NULLIF($2,''), $3, $4, $5, $6::jsonb, $7)`, db.NewID(), actorID, action, targetType, targetID, string(encoded), requestID(ctx))
	if err != nil {
		s.logger.Error("audit write failed", "action", action)
	}
}

func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	if p, ok := r.Context().Value(localPrincipalContextKey{}).(*Principal); ok && p != nil {
		principal := *p
		return &principal, nil
	}
	if header := r.Header.Get("Authorization"); header != "" {
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return nil, errUnauthenticated
		}
		token := parts[1]
		if len(token) < 24 || strings.ContainsAny(token, " \t\r\n") {
			return nil, errUnauthenticated
		}
		digest := HashToken(token, s.currentConfig().SessionSecret)
		var p Principal
		var status string
		var scopes []string
		err := s.db.SQL.QueryRowContext(r.Context(), `SELECT u.id,u.username,u.email,u.display_name,u.role,u.status,t.id,t.scopes
			FROM access_tokens t JOIN users u ON u.id=t.user_id
			WHERE t.token_hash=$1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())`, digest).
			Scan(&p.ID, &p.Username, &p.Email, &p.DisplayName, &p.Role, &status, &p.TokenID, &scopes)
		if err != nil || status != "active" {
			return nil, errUnauthenticated
		}
		p.Authenticated = true
		p.Scopes = scopes
		_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE access_tokens SET last_used_at=now() WHERE id=$1`, p.TokenID)
		return &p, nil
	}
	cookie, err := r.Cookie("hfproxy_session")
	if err != nil {
		return nil, errNoCredentials
	}
	if cookie.Value == "" {
		return nil, errUnauthenticated
	}
	digest := HashToken(cookie.Value, s.currentConfig().SessionSecret)
	var p Principal
	var status string
	err = s.db.SQL.QueryRowContext(r.Context(), `SELECT u.id,u.username,u.email,u.display_name,u.role,u.status
		FROM sessions ss JOIN users u ON u.id=ss.user_id
		WHERE ss.token_hash=$1 AND ss.expires_at>now()`, digest).
		Scan(&p.ID, &p.Username, &p.Email, &p.DisplayName, &p.Role, &status)
	if err != nil || status != "active" {
		return nil, errUnauthenticated
	}
	p.Authenticated = true
	_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE sessions SET last_seen_at=now(), expires_at=LEAST(created_at + $2::interval, now() + $1::interval) WHERE token_hash=$3`,
		s.currentConfig().SessionIdleTTL.String(), s.currentConfig().SessionAbsoluteTTL.String(), digest)
	return &p, nil
}

func hasBearer(r *http.Request) bool {
	parts := strings.Fields(r.Header.Get("Authorization"))
	return len(parts) == 2 && strings.EqualFold(parts[0], "Bearer")
}

func (s *Server) currentPrincipal(r *http.Request) *Principal {
	p, _ := s.authenticate(r)
	return p
}

func (s *Server) createSession(ctx context.Context, userID string, w http.ResponseWriter, r *http.Request) (string, error) {
	token := randomSecret(32)
	csrf := randomSecret(32)
	tokenHash := HashToken(token, s.currentConfig().SessionSecret)
	csrfHash := HashToken(csrf, s.currentConfig().SessionSecret)
	_, err := s.db.SQL.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES ($1,$2,$3,$4)`,
		tokenHash, userID, csrfHash, time.Now().Add(s.currentConfig().SessionIdleTTL))
	if err != nil {
		return "", err
	}
	for _, cookie := range newSessionCookies(token, csrf, s.currentConfig()) {
		http.SetCookie(w, cookie)
	}
	return csrf, nil
}

func newSessionCookies(token, csrf string, cfg config.Config) []*http.Cookie {
	secure := strings.HasPrefix(cfg.PublicURL, "https://") || strings.HasPrefix(cfg.OIDCRedirectURL, "https://")
	maxAge := int(cfg.SessionAbsoluteTTL.Seconds())
	return []*http.Cookie{
		{Name: "hfproxy_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge},
		{Name: "hfproxy_csrf", Value: csrf, Path: "/", Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge},
	}
}

func (s *Server) checkCSRF(r *http.Request) bool {
	csrfCookie, err := r.Cookie("hfproxy_csrf")
	if err != nil || csrfCookie.Value == "" {
		return false
	}
	provided := r.FormValue("csrf_token")
	if provided == "" {
		provided = r.Header.Get("X-CSRF-Token")
	}
	if provided == "" || !constantEqual(provided, csrfCookie.Value) {
		return false
	}
	cookie, err := r.Cookie("hfproxy_session")
	if err != nil {
		return false
	}
	digest := HashToken(cookie.Value, s.currentConfig().SessionSecret)
	var stored []byte
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT csrf_hash FROM sessions WHERE token_hash=$1 AND expires_at>now()`, digest).Scan(&stored); err != nil {
		return false
	}
	return constantEqualBytes(stored, HashToken(provided, s.currentConfig().SessionSecret))
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	p, err := s.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Provide a valid hfproxy token or sign in.")
		return nil, false
	}
	if p.Role != "admin" {
		writeProblem(w, r, http.StatusForbidden, "Forbidden", "Administrator role required.")
		return nil, false
	}
	return p, true
}

func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	p, err := s.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Provide a valid hfproxy token or sign in.")
		return nil, false
	}
	if p.Role != "admin" && p.Role != "operator" {
		writeProblem(w, r, http.StatusForbidden, "Forbidden", "Operator or administrator role required.")
		return nil, false
	}
	return p, true
}

var errUnauthenticated = errors.New("unauthenticated")
var errNoCredentials = errors.New("no credentials")
