package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/storage"
)

func (s *Server) apiV1(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		http.NotFound(w, r)
		return
	}
	if isMutation(r.Method) && !hasBearer(r) && !s.checkCSRF(r) {
		writeProblem(w, r, http.StatusForbidden, "CSRF validation failed", "Reload the page and retry the action.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if isMutation(r.Method) && hasBearer(r) {
		p, err := s.authenticate(r)
		if err != nil || p.TokenID == "" || (!hasScope(p, "write") && !hasScope(p, "admin")) {
			writeProblem(w, r, http.StatusForbidden, "Forbidden", "This token is read-only and cannot modify the administration API.")
			return
		}
	}
	// Match known paths before methods so callers receive 405 with the
	// endpoint's actual Allow header instead of an ambiguous 404.
	switch path {
	case "auth/providers", "me", "healthz", "stats", "downloads/active", "cache/entries", "audit-events", "settings/public":
		if r.Method != http.MethodGet {
			methodNotAllowedWith(w, "GET")
			return
		}
	case "repos", "users", "groups":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			methodNotAllowedWith(w, "GET, POST")
			return
		}
	case "cache/purge":
		if r.Method != http.MethodPost {
			methodNotAllowedWith(w, "POST")
			return
		}
	case "settings":
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			methodNotAllowedWith(w, "GET, PUT")
			return
		}
	case "settings/test-s3", "settings/test-hub":
		if r.Method != http.MethodPost {
			methodNotAllowedWith(w, "POST")
			return
		}
	case "users/", "groups/":
		// Handled below; this case documents the prefix-only routes.
	}
	if strings.HasPrefix(path, "repos/") {
		parts := strings.Split(strings.TrimPrefix(path, "repos/"), "/")
		allow := "GET, PATCH, DELETE"
		if len(parts) == 2 && parts[1] == "grants" {
			allow = "GET, POST, DELETE"
		} else if len(parts) == 2 && parts[1] == "preload" {
			allow = "POST"
		} else if len(parts) != 1 {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
			return
		}
		if !strings.Contains(allow, r.Method) {
			methodNotAllowedWith(w, allow)
			return
		}
	}
	if strings.HasPrefix(path, "users/") && strings.Count(strings.TrimPrefix(path, "users/"), "/") == 0 && r.Method != http.MethodPatch {
		methodNotAllowedWith(w, http.MethodPatch)
		return
	}
	if strings.HasPrefix(path, "groups/") && strings.Count(strings.TrimPrefix(path, "groups/"), "/") == 0 && r.Method != http.MethodGet && r.Method != http.MethodPatch {
		methodNotAllowedWith(w, "GET, PATCH")
		return
	}
	switch {
	case path == "auth/providers" && r.Method == http.MethodGet:
		providers := map[string]any{"local": s.currentConfig().AuthMode == "local", "oidc": s.currentOIDCConfig() != nil, "ldap": s.currentConfig().LDAPURL != ""}
		writeJSON(w, http.StatusOK, providers)
	case path == "me" && r.Method == http.MethodGet:
		p, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view your profile.")
			return
		}
		writeJSON(w, http.StatusOK, p)
	case path == "healthz" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case path == "stats" && r.Method == http.MethodGet:
		s.apiStats(w, r)
	case path == "downloads/active" && r.Method == http.MethodGet:
		s.apiActiveDownloads(w, r)
	case path == "repos":
		s.apiRepos(w, r)
	case strings.HasPrefix(path, "repos/"):
		s.apiRepo(w, r, strings.TrimPrefix(path, "repos/"))
	case path == "cache/entries" && r.Method == http.MethodGet:
		s.apiCacheEntries(w, r)
	case strings.HasPrefix(path, "cache/entries/"):
		s.apiCacheEntry(w, r, strings.TrimPrefix(path, "cache/entries/"))
	case path == "cache/purge" && r.Method == http.MethodPost:
		s.apiPurge(w, r)
	case path == "users" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.apiUsers(w, r)
	case strings.HasPrefix(path, "users/") && r.Method == http.MethodPatch:
		s.apiUserPatch(w, r, strings.TrimPrefix(path, "users/"))
	case path == "groups" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.apiGroups(w, r)
	case strings.HasPrefix(path, "groups/"):
		if strings.Count(path, "/") != 1 {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
			return
		}
		s.apiGroup(w, r, strings.TrimPrefix(path, "groups/"))
	case path == "audit-events" && r.Method == http.MethodGet:
		s.apiAudit(w, r)
	case path == "settings/public" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"auth_mode": s.currentConfig().AuthMode, "anonymous_public_read": s.currentConfig().AnonymousPublicRead, "oidc_enabled": s.currentOIDCConfig() != nil, "ldap_enabled": s.currentConfig().LDAPURL != "", "ldap_auto_provision": s.currentConfig().LDAPAutoProvision, "s3_enabled": true, "hub_base_url": s.currentConfig().HFBaseURL, "presigned_urls": false})
	case path == "settings":
		s.apiSettings(w, r)
	case path == "settings/test-s3" && r.Method == http.MethodPost:
		s.apiTestS3(w, r)
	case path == "settings/test-hub" && r.Method == http.MethodPost:
		s.apiTestHub(w, r)
	default:
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
	}
}

func isMutation(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

type settingsResponse struct {
	PublicURL              string   `json:"public_url"`
	HFBaseURL              string   `json:"hf_base_url"`
	HFProxySet             bool     `json:"hf_proxy_set"`
	HFAllowedHosts         []string `json:"hf_allowed_hosts"`
	HFTimeout              string   `json:"hf_timeout"`
	HFTokenSet             bool     `json:"hf_token_set"`
	S3Endpoint             string   `json:"s3_endpoint"`
	S3Region               string   `json:"s3_region"`
	S3Bucket               string   `json:"s3_bucket"`
	S3Prefix               string   `json:"s3_prefix"`
	S3ForcePathStyle       bool     `json:"s3_force_path_style"`
	S3ServerSideEncryption bool     `json:"s3_server_side_encryption"`
	S3AccessKeySet         bool     `json:"s3_access_key_set"`
	S3SecretKeySet         bool     `json:"s3_secret_key_set"`
	AuthMode               string   `json:"auth_mode"`
	AnonymousPublicRead    bool     `json:"anonymous_public_read"`
	SessionIdleTTL         string   `json:"session_idle_ttl"`
	SessionAbsoluteTTL     string   `json:"session_absolute_ttl"`
	CacheMaxFileSize       int64    `json:"cache_max_file_size"`
	CacheRefTTL            string   `json:"cache_ref_ttl"`
	CacheNegativeTTL       string   `json:"cache_negative_ttl"`
	CacheGCGrace           string   `json:"cache_gc_grace"`
	OIDCIssuer             string   `json:"oidc_issuer"`
	OIDCClientID           string   `json:"oidc_client_id"`
	OIDCClientSecretSet    bool     `json:"oidc_client_secret_set"`
	OIDCRedirectURL        string   `json:"oidc_redirect_url"`
	OIDCAutoProvision      bool     `json:"oidc_auto_provision"`
	LDAPURL                string   `json:"ldap_url"`
	LDAPStartTLS           bool     `json:"ldap_starttls"`
	LDAPBindDN             string   `json:"ldap_bind_dn"`
	LDAPBindPasswordSet    bool     `json:"ldap_bind_password_set"`
	LDAPBaseDN             string   `json:"ldap_base_dn"`
	LDAPUserFilter         string   `json:"ldap_user_filter"`
	LDAPAutoProvision      bool     `json:"ldap_auto_provision"`
}

type settingsInput struct {
	PublicURL              string   `json:"public_url"`
	HFBaseURL              string   `json:"hf_base_url"`
	HFProxyURL             string   `json:"hf_proxy_url"`
	ClearHFProxy           bool     `json:"clear_hf_proxy"`
	HFAllowedHosts         []string `json:"hf_allowed_hosts"`
	HFTimeout              string   `json:"hf_timeout"`
	HFToken                string   `json:"hf_token"`
	ClearHFToken           bool     `json:"clear_hf_token"`
	S3Endpoint             string   `json:"s3_endpoint"`
	S3Region               string   `json:"s3_region"`
	S3Bucket               string   `json:"s3_bucket"`
	S3Prefix               string   `json:"s3_prefix"`
	S3ForcePathStyle       bool     `json:"s3_force_path_style"`
	S3ServerSideEncryption bool     `json:"s3_server_side_encryption"`
	S3AccessKey            string   `json:"s3_access_key"`
	S3SecretKey            string   `json:"s3_secret_key"`
	ClearS3Credentials     bool     `json:"clear_s3_credentials"`
	AuthMode               string   `json:"auth_mode"`
	AnonymousPublicRead    bool     `json:"anonymous_public_read"`
	SessionIdleTTL         string   `json:"session_idle_ttl"`
	SessionAbsoluteTTL     string   `json:"session_absolute_ttl"`
	CacheMaxFileSize       int64    `json:"cache_max_file_size"`
	CacheRefTTL            string   `json:"cache_ref_ttl"`
	CacheNegativeTTL       string   `json:"cache_negative_ttl"`
	CacheGCGrace           string   `json:"cache_gc_grace"`
	OIDCIssuer             string   `json:"oidc_issuer"`
	OIDCClientID           string   `json:"oidc_client_id"`
	OIDCClientSecret       string   `json:"oidc_client_secret"`
	ClearOIDCSecret        bool     `json:"clear_oidc_client_secret"`
	OIDCRedirectURL        string   `json:"oidc_redirect_url"`
	OIDCAutoProvision      bool     `json:"oidc_auto_provision"`
	LDAPURL                string   `json:"ldap_url"`
	LDAPStartTLS           bool     `json:"ldap_starttls"`
	LDAPBindDN             string   `json:"ldap_bind_dn"`
	LDAPBindPassword       string   `json:"ldap_bind_password"`
	ClearLDAPPassword      bool     `json:"clear_ldap_bind_password"`
	LDAPBaseDN             string   `json:"ldap_base_dn"`
	LDAPUserFilter         string   `json:"ldap_user_filter"`
	LDAPAutoProvision      bool     `json:"ldap_auto_provision"`
}

func settingsView(cfg config.Config) settingsResponse {
	return settingsResponse{
		PublicURL: cfg.PublicURL, HFBaseURL: cfg.HFBaseURL, HFProxySet: cfg.HFProxyURL != "", HFAllowedHosts: cfg.HFAllowedHosts, HFTimeout: cfg.HFTimeout.String(), HFTokenSet: cfg.HFToken != "",
		S3Endpoint: cfg.S3Endpoint, S3Region: cfg.S3Region, S3Bucket: cfg.S3Bucket, S3Prefix: cfg.S3Prefix, S3ForcePathStyle: cfg.S3ForcePathStyle, S3ServerSideEncryption: cfg.S3ServerSideEncryption,
		S3AccessKeySet: cfg.S3AccessKey != "", S3SecretKeySet: cfg.S3SecretKey != "", AuthMode: cfg.AuthMode, AnonymousPublicRead: cfg.AnonymousPublicRead,
		SessionIdleTTL: cfg.SessionIdleTTL.String(), SessionAbsoluteTTL: cfg.SessionAbsoluteTTL.String(), CacheMaxFileSize: cfg.CacheMaxFileSize,
		CacheRefTTL: cfg.CacheRefTTL.String(), CacheNegativeTTL: cfg.CacheNegativeTTL.String(), CacheGCGrace: cfg.CacheGCGrace.String(),
		OIDCIssuer: cfg.OIDCIssuer, OIDCClientID: cfg.OIDCClientID, OIDCClientSecretSet: cfg.OIDCClientSecret != "", OIDCRedirectURL: cfg.OIDCRedirectURL,
		OIDCAutoProvision: cfg.OIDCAutoProvision, LDAPURL: cfg.LDAPURL, LDAPStartTLS: cfg.LDAPStartTLS, LDAPBindDN: cfg.LDAPBindDN,
		LDAPBindPasswordSet: cfg.LDAPBindPassword != "", LDAPBaseDN: cfg.LDAPBaseDN, LDAPUserFilter: cfg.LDAPUserFilter, LDAPAutoProvision: cfg.LDAPAutoProvision,
	}
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, settingsView(s.currentConfig()))
	case http.MethodPut:
		var input settingsInput
		if !decodeJSON(w, r, &input) {
			return
		}
		cfg, err := input.apply(s.currentConfig())
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid settings", err.Error())
			return
		}
		if err := s.applyConfig(r.Context(), cfg, p.ID); err != nil {
			s.logger.Warn("settings update failed", "request_id", requestID(r.Context()))
			writeProblem(w, r, http.StatusBadRequest, "Settings could not be applied", "Check provider availability and configuration, then retry.")
			return
		}
		fields := []string{"public_url", "huggingface", "s3", "authentication", "cache"}
		s.audit(r.Context(), p.ID, "settings.update", "settings", "runtime", map[string]any{"updated_sections": fields})
		writeJSON(w, http.StatusOK, settingsView(cfg))
	case http.MethodDelete:
		writeProblem(w, r, http.StatusNotImplemented, "Reset unavailable", "Edit settings directly; environment settings are not reloaded while the process is running.")
	default:
		methodNotAllowed(w)
	}
}

func (in settingsInput) apply(cfg config.Config) (config.Config, error) {
	parseDuration := func(name, value string, current time.Duration) (time.Duration, error) {
		if value == "" {
			return current, nil
		}
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("%s must be a valid Go duration", name)
		}
		return parsed, nil
	}
	var err error
	cfg.PublicURL, cfg.HFBaseURL = strings.TrimSpace(in.PublicURL), strings.TrimSpace(in.HFBaseURL)
	var hostErr error
	cfg.HFAllowedHosts, hostErr = config.NormalizeAllowedHosts(in.HFAllowedHosts)
	if hostErr != nil {
		return config.Config{}, hostErr
	}
	if in.ClearHFProxy {
		cfg.HFProxyURL = ""
	} else if proxyURL := strings.TrimSpace(in.HFProxyURL); proxyURL != "" {
		cfg.HFProxyURL = proxyURL
	}
	cfg.HFTimeout, err = parseDuration("HF timeout", in.HFTimeout, cfg.HFTimeout)
	if err != nil {
		return config.Config{}, err
	}
	cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption = strings.TrimSpace(in.S3Endpoint), strings.TrimSpace(in.S3Region), strings.TrimSpace(in.S3Bucket), strings.Trim(in.S3Prefix, "/"), in.S3ForcePathStyle, in.S3ServerSideEncryption
	cfg.AuthMode, cfg.AnonymousPublicRead = in.AuthMode, in.AnonymousPublicRead
	cfg.CacheMaxFileSize = in.CacheMaxFileSize
	cfg.SessionIdleTTL, err = parseDuration("session idle TTL", in.SessionIdleTTL, cfg.SessionIdleTTL)
	if err != nil {
		return config.Config{}, err
	}
	cfg.SessionAbsoluteTTL, err = parseDuration("session absolute TTL", in.SessionAbsoluteTTL, cfg.SessionAbsoluteTTL)
	if err != nil {
		return config.Config{}, err
	}
	cfg.CacheRefTTL, err = parseDuration("reference TTL", in.CacheRefTTL, cfg.CacheRefTTL)
	if err != nil {
		return config.Config{}, err
	}
	cfg.CacheNegativeTTL, err = parseDuration("negative cache TTL", in.CacheNegativeTTL, cfg.CacheNegativeTTL)
	if err != nil {
		return config.Config{}, err
	}
	cfg.CacheGCGrace, err = parseDuration("cache GC grace", in.CacheGCGrace, cfg.CacheGCGrace)
	if err != nil {
		return config.Config{}, err
	}
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCRedirectURL, cfg.OIDCAutoProvision = strings.TrimSpace(in.OIDCIssuer), strings.TrimSpace(in.OIDCClientID), strings.TrimSpace(in.OIDCRedirectURL), in.OIDCAutoProvision
	cfg.LDAPURL, cfg.LDAPStartTLS, cfg.LDAPBindDN = strings.TrimSpace(in.LDAPURL), in.LDAPStartTLS, strings.TrimSpace(in.LDAPBindDN)
	cfg.LDAPBaseDN, cfg.LDAPUserFilter, cfg.LDAPAutoProvision = strings.TrimSpace(in.LDAPBaseDN), strings.TrimSpace(in.LDAPUserFilter), in.LDAPAutoProvision
	if in.ClearHFToken {
		cfg.HFToken = ""
	} else if in.HFToken != "" {
		cfg.HFToken = in.HFToken
	}
	if in.ClearS3Credentials {
		cfg.S3AccessKey, cfg.S3SecretKey = "", ""
	} else {
		if in.S3AccessKey != "" {
			cfg.S3AccessKey = in.S3AccessKey
		}
		if in.S3SecretKey != "" {
			cfg.S3SecretKey = in.S3SecretKey
		}
	}
	if in.ClearOIDCSecret {
		cfg.OIDCClientSecret = ""
	} else if in.OIDCClientSecret != "" {
		cfg.OIDCClientSecret = in.OIDCClientSecret
	}
	if in.ClearLDAPPassword {
		cfg.LDAPBindPassword = ""
	} else if in.LDAPBindPassword != "" {
		cfg.LDAPBindPassword = in.LDAPBindPassword
	}
	if cfg.S3Bucket == "" {
		return config.Config{}, fmt.Errorf("S3 bucket is required before applying settings")
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func (s *Server) apiTestS3(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var input struct {
		Endpoint             string `json:"s3_endpoint"`
		Region               string `json:"s3_region"`
		Bucket               string `json:"s3_bucket"`
		Prefix               string `json:"s3_prefix"`
		ForcePathStyle       bool   `json:"s3_force_path_style"`
		ServerSideEncryption bool   `json:"s3_server_side_encryption"`
		AccessKey            string `json:"s3_access_key"`
		SecretKey            string `json:"s3_secret_key"`
		Clear                bool   `json:"clear_s3_credentials"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	cfg := s.currentConfig()
	cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption = strings.TrimSpace(input.Endpoint), strings.TrimSpace(input.Region), strings.TrimSpace(input.Bucket), strings.Trim(input.Prefix, "/"), input.ForcePathStyle, input.ServerSideEncryption
	if cfg.S3Bucket == "" {
		writeProblem(w, r, http.StatusBadRequest, "Invalid S3 settings", "An S3 bucket is required.")
		return
	}
	if input.Clear {
		cfg.S3AccessKey, cfg.S3SecretKey = "", ""
	} else {
		if input.AccessKey != "" {
			cfg.S3AccessKey = input.AccessKey
		}
		if input.SecretKey != "" {
			cfg.S3SecretKey = input.SecretKey
		}
	}
	if err := cfg.Validate(); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid S3 settings", err.Error())
		return
	}
	objects, err := storage.New(r.Context(), cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Prefix, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3ForcePathStyle, cfg.S3ServerSideEncryption)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid S3 settings", "Check the endpoint, bucket, region, and credentials.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := objects.Ready(ctx); err != nil {
		writeProblem(w, r, http.StatusBadGateway, "S3 connection failed", "Check the endpoint, bucket, region, and credentials.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "s3"})
}

func (s *Server) apiTestHub(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var input struct {
		BaseURL    string `json:"hf_base_url"`
		ProxyURL   string `json:"hf_proxy_url"`
		ClearProxy bool   `json:"clear_hf_proxy"`
		Token      string `json:"hf_token"`
		Clear      bool   `json:"clear_hf_token"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	cfg := s.currentConfig()
	cfg.HFBaseURL = strings.TrimSpace(input.BaseURL)
	if input.ClearProxy {
		cfg.HFProxyURL = ""
	} else if proxyURL := strings.TrimSpace(input.ProxyURL); proxyURL != "" {
		cfg.HFProxyURL = proxyURL
	}
	if input.Clear {
		cfg.HFToken = ""
	} else if input.Token != "" {
		cfg.HFToken = input.Token
	}
	if err := cfg.Validate(); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid Hub settings", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.HFBaseURL, "/")+"/api/whoami-v2", nil)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid Hub settings", "Check the Hub URL.")
		return
	}
	if cfg.HFToken != "" {
		configuredBase, _ := url.Parse(s.currentConfig().HFBaseURL)
		candidateBase, _ := url.Parse(cfg.HFBaseURL)
		if configuredBase != nil && candidateBase != nil && strings.EqualFold(configuredBase.Hostname(), candidateBase.Hostname()) {
			req.Header.Set("Authorization", "Bearer "+cfg.HFToken)
		}
	}
	resp, err := newHFHTTPClient(cfg).Do(req)
	if err != nil {
		writeProblem(w, r, http.StatusBadGateway, "Hugging Face connection failed", "Check the Hub URL, proxy scheme and credentials, network access, and token.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && cfg.HFToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "huggingface", "token_configured": false})
		return
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		writeProblem(w, r, http.StatusBadGateway, "Hugging Face rejected the token", "The Hub is reachable, but the configured token is missing or does not have access.")
		return
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		writeProblem(w, r, http.StatusBadGateway, "Proxy authentication failed", "Check the configured proxy credentials.")
		return
	}
	if resp.StatusCode >= 500 {
		writeProblem(w, r, http.StatusBadGateway, "Hugging Face is unavailable", "The Hub returned a server error.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "huggingface"})
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view cache statistics.")
		return
	}
	var repos, artifacts, totalBytes, hits, misses int64
	visibility := `($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))`
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM repositories r WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&repos); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&artifacts, &totalBytes); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FILTER(WHERE d.status='hit'),COUNT(*) FILTER(WHERE d.status='miss') FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&hits, &misses); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	var active int64
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE d.status='downloading' AND r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&active)
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos, "artifacts": artifacts, "cache_bytes": totalBytes, "cache_size": formatBytes(totalBytes), "cache_hits": hits, "cache_misses": misses, "hit_ratio": func() float64 {
		if hits+misses == 0 {
			return 0
		}
		return float64(hits) / float64(hits+misses)
	}(), "downloads_in_progress": active, "actor_role": p.Role})
}

func (s *Server) apiActiveDownloads(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view active downloads.")
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT d.id,r.repo_type,r.repo_id,d.path,d.bytes,d.started_at
		FROM downloads d JOIN repositories r ON r.id=d.repository_id
		WHERE d.status='downloading' AND r.deleted_at IS NULL
		AND ($1='admin' OR r.access_policy='public'
			OR (r.access_policy='allowlist' AND (
			EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2)
			OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2)))
		ORDER BY d.started_at DESC LIMIT 50`, p.Role, p.ID)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read active downloads.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, repoType, repoID, path string
		var bytes int64
		var started time.Time
		if err := rows.Scan(&id, &repoType, &repoID, &path, &bytes, &started); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read active downloads.")
			return
		}
		items = append(items, map[string]any{"id": id, "repo_type": repoType, "repo_id": repoID, "path": path, "bytes": bytes, "started_at": started})
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read active downloads.")
		return
	}
	preloads, err := s.preloadSnapshots(r.Context(), p)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read preload status.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "preloads": preloads})
}

func (s *Server) apiRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, 401, "Authentication required", "Sign in to view repository policies.")
			return
		}
		limit := pageLimit(r.URL.Query().Get("limit"))
		args := []any{p.Role, p.ID, limit + 1}
		cursorWhere := ""
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			parts, ok := decodeListCursor(cursor, 3)
			if !ok {
				writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The repository cursor is invalid.")
				return
			}
			cursorWhere = " AND (r.repo_type,r.repo_id,r.id) > ($3,$4,$5)"
			args = []any{p.Role, p.ID, parts[0], parts[1], parts[2], limit + 1}
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT r.id,r.repo_type,r.repo_id,r.access_policy,r.created_at,r.updated_at
			FROM repositories r WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))`+cursorWhere+`
			ORDER BY r.repo_type,r.repo_id,r.id LIMIT $`+strconv.Itoa(len(args)), args...)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list repositories.")
			return
		}
		defer rows.Close()
		result := []map[string]any{}
		for rows.Next() {
			var id, t, repo, policy string
			var created, updated time.Time
			if rows.Scan(&id, &t, &repo, &policy, &created, &updated) == nil {
				result = append(result, map[string]any{"id": id, "repo_type": t, "repo_id": repo, "access_policy": policy, "created_at": created, "updated_at": updated})
			}
		}
		next := ""
		if len(result) > limit {
			last := result[limit-1]
			next = encodeListCursor(last["repo_type"].(string), last["repo_id"].(string), last["id"].(string))
			result = result[:limit]
		}
		writeJSON(w, 200, map[string]any{"items": result, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "next_cursor": next})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		RepoType     string `json:"repo_type"`
		RepoID       string `json:"repo_id"`
		AccessPolicy string `json:"access_policy"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if !validRepoType(input.RepoType) || !validRepoID(input.RepoID) || (input.AccessPolicy != "public" && input.AccessPolicy != "allowlist" && input.AccessPolicy != "admin_only") {
		writeProblem(w, r, 400, "Invalid repository", "Provide a valid repo_type, repo_id, and access_policy.")
		return
	}
	var id string
	err := s.db.SQL.QueryRowContext(r.Context(), `INSERT INTO repositories(id,repo_type,repo_id,access_policy,created_by) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(repo_type,repo_id) DO UPDATE SET access_policy=EXCLUDED.access_policy,deleted_at=NULL,updated_at=now() RETURNING id`, db.NewID(), input.RepoType, input.RepoID, input.AccessPolicy, p.ID).Scan(&id)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to save repository policy.")
		return
	}
	s.audit(r.Context(), p.ID, "repository.upsert", "repository", id, map[string]any{"repo_type": input.RepoType, "repo_id": input.RepoID, "access_policy": input.AccessPolicy})
	writeJSON(w, 201, map[string]string{"id": id, "repo_type": input.RepoType, "repo_id": input.RepoID, "access_policy": input.AccessPolicy})
}

func (s *Server) apiRepo(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 2 && parts[1] == "grants" {
		s.apiRepoGrants(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "preload" && r.Method == http.MethodPost {
		s.startRepoPreload(w, r, parts[0])
		return
	}
	if len(parts) != 1 {
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		var repoType, repoID, policy string
		var created, updated time.Time
		err := s.db.SQL.QueryRowContext(r.Context(), `SELECT repo_type,repo_id,access_policy,created_at,updated_at FROM repositories WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&repoType, &repoID, &policy, &created, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to load repository.")
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "repo_type": repoType, "repo_id": repoID, "access_policy": policy, "created_at": created, "updated_at": updated})
	case http.MethodPatch:
		var input struct {
			AccessPolicy string `json:"access_policy"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.AccessPolicy != "public" && input.AccessPolicy != "allowlist" && input.AccessPolicy != "admin_only" {
			writeProblem(w, r, 400, "Invalid policy", "access_policy must be public, allowlist, or admin_only.")
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE repositories SET access_policy=$2,updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id, input.AccessPolicy)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to update policy.")
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.policy.update", "repository", id, map[string]any{"access_policy": input.AccessPolicy})
		writeJSON(w, 200, map[string]string{"id": id, "access_policy": input.AccessPolicy})
	case http.MethodDelete:
		tx, err := s.db.SQL.BeginTx(r.Context(), nil)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
			return
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(r.Context(), `UPDATE repositories SET deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM repo_grants WHERE repository_id=$1`, id); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to remove repository grants.")
			return
		}
		if err := tx.Commit(); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.delete", "repository", id, nil)
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiRepoGrants(w http.ResponseWriter, r *http.Request, repoID string) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT subject_type,subject_id,permission FROM repo_grants WHERE repository_id=$1 ORDER BY subject_type,subject_id`, repoID)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list grants.")
			return
		}
		defer rows.Close()
		items := []map[string]string{}
		for rows.Next() {
			var a, b, c string
			if rows.Scan(&a, &b, &c) == nil {
				items = append(items, map[string]string{"subject_type": a, "subject_id": b, "permission": c})
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			SubjectType string `json:"subject_type"`
			SubjectID   string `json:"subject_id"`
			Permission  string `json:"permission"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if (input.SubjectType != "user" && input.SubjectType != "group") || input.SubjectID == "" || (input.Permission != "read" && input.Permission != "write" && input.Permission != "admin") {
			writeProblem(w, r, 400, "Invalid grant", "Provide a user or group subject and a valid permission.")
			return
		}
		var repositoryExists bool
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=$1 AND deleted_at IS NULL)`, repoID).Scan(&repositoryExists); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to validate the repository.")
			return
		}
		if !repositoryExists {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		table := "users"
		if input.SubjectType == "group" {
			table = "groups"
		}
		var subjectExists bool
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, input.SubjectID).Scan(&subjectExists); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to validate the grant subject.")
			return
		}
		if !subjectExists {
			writeProblem(w, r, 404, "Grant subject not found", "The selected user or group does not exist.")
			return
		}
		_, err := s.db.SQL.ExecContext(r.Context(), `INSERT INTO repo_grants(repository_id,subject_type,subject_id,permission) VALUES($1,$2,$3,$4) ON CONFLICT(repository_id,subject_type,subject_id) DO UPDATE SET permission=EXCLUDED.permission`, repoID, input.SubjectType, input.SubjectID, input.Permission)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to save grant.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.grant.upsert", "repository", repoID, map[string]any{"subject_type": input.SubjectType, "subject_id": input.SubjectID, "permission": input.Permission})
		writeJSON(w, 201, input)
		return
	}
	if r.Method == http.MethodDelete {
		subjectType := r.URL.Query().Get("subject_type")
		subjectID := r.URL.Query().Get("subject_id")
		if subjectType != "user" && subjectType != "group" || subjectID == "" {
			writeProblem(w, r, 400, "Invalid grant", "subject_type and subject_id are required.")
			return
		}
		_, err := s.db.SQL.ExecContext(r.Context(), `DELETE FROM repo_grants WHERE repository_id=$1 AND subject_type=$2 AND subject_id=$3`, repoID, subjectType, subjectID)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to remove grant.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.grant.delete", "repository", repoID, map[string]any{"subject_type": subjectType, "subject_id": subjectID})
		w.WriteHeader(204)
		return
	}
	methodNotAllowed(w)
}

func (s *Server) apiCacheEntries(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, 401, "Authentication required", "Sign in to view cache metadata.")
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" {
		writeProblem(w, r, 400, "Invalid cursor", "Opaque cursors are not yet accepted; omit cursor to list the first page.")
		return
	}
	repoFilter := r.URL.Query().Get("repo_id")
	pathFilter := r.URL.Query().Get("path")
	pathPattern := escapeLikePattern(pathFilter)
	query := `SELECT a.id,r.repo_type,r.repo_id,a.commit_sha,a.path,b.size_bytes,b.sha256,a.etag,a.cached_at,a.last_accessed_at
		FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id
		WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))
		AND ($3='' OR r.repo_id=$3) AND ($4='' OR a.path ILIKE '%'||$4||'%' ESCAPE '\\') ORDER BY a.cached_at DESC LIMIT $5`
	rows, err := s.db.SQL.QueryContext(r.Context(), query, p.Role, p.ID, repoFilter, pathPattern, limit)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to search cache.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, t, repo, commit, path, sha, etag string
		var size int64
		var cached, accessed time.Time
		if rows.Scan(&id, &t, &repo, &commit, &path, &size, &sha, &etag, &cached, &accessed) == nil {
			items = append(items, map[string]any{"id": id, "repo_type": t, "repo_id": repo, "commit_sha": commit, "path": path, "size_bytes": size, "sha256": sha, "etag": etag, "cached_at": cached, "last_accessed_at": accessed})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "next_cursor": ""})
}

func (s *Server) apiCacheEntry(w http.ResponseWriter, r *http.Request, id string) {
	if !strings.HasSuffix(id, "/purge") {
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowedWith(w, http.MethodPost)
		return
	}
	id = strings.TrimSuffix(id, "/purge")
	p, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	if err := s.purgeArtifact(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, r, 404, "Cache entry not found", "The cache entry does not exist.")
			return
		}
		writeProblem(w, r, 500, "Purge failed", "Unable to purge the cache entry.")
		return
	}
	s.audit(r.Context(), p.ID, "cache.entry.purge", "artifact", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiPurge(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeProblem(w, r, 400, "Invalid request", "Unable to read purge filters.")
		return
	}
	dry := r.URL.Query().Get("dry_run") == "true" || r.FormValue("dry_run") == "true"
	confirm := r.URL.Query().Get("confirm") == "true" || r.FormValue("confirm") == "true"
	repoID := r.URL.Query().Get("repo_id")
	path := r.URL.Query().Get("path")
	pathPattern := escapeLikePattern(path)
	var count int64
	var size int64
	err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR a.path ILIKE '%'||$2||'%' ESCAPE '\\')`, repoID, pathPattern).Scan(&count, &size)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to estimate purge impact.")
		return
	}
	if dry || !confirm {
		writeJSON(w, 200, map[string]any{"dry_run": true, "entries": count, "bytes": size, "size": formatBytes(size), "confirmation_required": true})
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT a.id FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR a.path ILIKE '%'||$2||'%' ESCAPE '\\')`, repoID, pathPattern)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to find cache entries.")
		return
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
		if err := s.purgeArtifact(r.Context(), id); err != nil {
			s.logger.Warn("cache purge entry failed")
		}
	}
	s.audit(r.Context(), p.ID, "cache.purge", "cache", "filtered", map[string]any{"entries": len(ids), "repo_id": repoID, "path_filter": path})
	writeJSON(w, 200, map[string]any{"dry_run": false, "entries_purged": len(ids), "estimated_bytes": size})
}

func (s *Server) purgeArtifact(ctx context.Context, id string) error {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var blobID string
	err = tx.QueryRowContext(ctx, `SELECT b.id FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.id=$1 FOR UPDATE OF a`, id).Scan(&blobID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1`, blobID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Keep the blob row/object as an orphan until a grace-period GC removes it.
	// This prevents a purge racing a re-download from deleting the new object.
	return nil
}

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			Username    string `json:"username"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
			Role        string `json:"role"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		input.Username = strings.TrimSpace(input.Username)
		input.Email = strings.TrimSpace(input.Email)
		input.DisplayName = strings.TrimSpace(input.DisplayName)
		if input.Role == "" {
			input.Role = "reader"
		}
		if !validLocalUsername(input.Username) {
			writeProblem(w, r, http.StatusBadRequest, "Invalid username", "Use 1 to 64 letters, digits, dots, underscores, or hyphens.")
			return
		}
		if len(input.Email) > 254 || (input.Email != "" && !validEmail(input.Email)) || len(input.DisplayName) > 200 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid profile", "Check the email address and keep the display name under 200 characters.")
			return
		}
		if input.Role != "admin" && input.Role != "operator" && input.Role != "reader" {
			writeProblem(w, r, http.StatusBadRequest, "Invalid role", "role must be admin, operator, or reader.")
			return
		}
		hash, err := HashPassword(input.Password)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid password", "Password must contain 12 to 1024 bytes.")
			return
		}
		id := db.NewID()
		_, err = s.db.SQL.ExecContext(r.Context(), `INSERT INTO users(id,username,email,display_name,password_hash,role) VALUES($1,$2,$3,$4,$5,$6)`, id, input.Username, input.Email, input.DisplayName, hash, input.Role)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				writeProblem(w, r, http.StatusConflict, "Username already exists", "Choose a different username.")
				return
			}
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create user.")
			return
		}
		s.audit(r.Context(), p.ID, "user.create", "user", id, map[string]any{"username": input.Username, "role": input.Role})
		writeJSON(w, http.StatusCreated, map[string]string{"id": id, "username": input.Username, "role": input.Role, "status": "active"})
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	args := []any{limit + 1}
	where := ""
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		parts, ok := decodeListCursor(cursor, 2)
		if !ok {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The user cursor is invalid.")
			return
		}
		where = "WHERE (username,id) > ($1,$2)"
		args = []any{parts[0], parts[1], limit + 1}
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,username,email,display_name,role,status,created_at FROM users `+where+` ORDER BY username,id LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to list users.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, user, email, name, role, status string
		var created time.Time
		if rows.Scan(&id, &user, &email, &name, &role, &status, &created) == nil {
			items = append(items, map[string]any{"id": id, "username": user, "email": email, "display_name": name, "role": role, "status": status, "created_at": created})
		}
	}
	next := ""
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		next = encodeListCursor(last["username"].(string), last["id"].(string))
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "next_cursor": next})
}

func validLocalUsername(username string) bool {
	if username == "" || len(username) > 64 {
		return false
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func validEmail(address string) bool {
	parsed, err := mail.ParseAddress(address)
	return err == nil && parsed.Address == address
}

func (s *Server) apiUserPatch(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Role != "" && input.Role != "admin" && input.Role != "operator" && input.Role != "reader" {
		writeProblem(w, r, 400, "Invalid role", "role must be admin, operator, or reader.")
		return
	}
	if input.Status != "" && input.Status != "active" && input.Status != "disabled" {
		writeProblem(w, r, 400, "Invalid status", "status must be active or disabled.")
		return
	}
	if input.Role == "" && input.Status == "" {
		writeProblem(w, r, 400, "Empty update", "Set role or status.")
		return
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	defer tx.Rollback()
	var currentRole, currentStatus string
	if err := tx.QueryRowContext(r.Context(), `SELECT role,status FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&currentRole, &currentStatus); errors.Is(err, sql.ErrNoRows) {
		writeProblem(w, r, 404, "User not found", "The user does not exist.")
		return
	} else if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	if currentRole == "admin" && currentStatus == "active" && (input.Role == "reader" || input.Role == "operator" || input.Status == "disabled") {
		var activeAdmins int
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&activeAdmins); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to validate administrator safety.")
			return
		}
		if activeAdmins <= 1 {
			writeProblem(w, r, http.StatusConflict, "Last administrator", "At least one active administrator must remain.")
			return
		}
	}
	result, err := tx.ExecContext(r.Context(), `UPDATE users SET role=COALESCE(NULLIF($2,''),role),status=COALESCE(NULLIF($3,''),status),updated_at=now() WHERE id=$1`, id, input.Role, input.Status)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeProblem(w, r, 404, "User not found", "The user does not exist.")
		return
	}
	if input.Status == "disabled" {
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, id)
		_, _ = tx.ExecContext(r.Context(), `UPDATE access_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id)
	}
	if err := tx.Commit(); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	s.audit(r.Context(), p.ID, "user.update", "user", id, map[string]any{"role": input.Role, "status": input.Status})
	writeJSON(w, 200, map[string]string{"id": id, "role": input.Role, "status": input.Status})
}

func (s *Server) apiGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.requireAdmin(w, r); !ok {
			return
		}
		limit := pageLimit(r.URL.Query().Get("limit"))
		args := []any{limit + 1}
		where := ""
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			parts, ok := decodeListCursor(cursor, 2)
			if !ok {
				writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The group cursor is invalid.")
				return
			}
			where = "WHERE (g.name,g.id) > ($1,$2)"
			args = []any{parts[0], parts[1], limit + 1}
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT g.id,g.name,g.source,COUNT(m.user_id) FROM groups g LEFT JOIN group_members m ON m.group_id=g.id `+where+` GROUP BY g.id ORDER BY g.name,g.id LIMIT $`+strconv.Itoa(len(args)), args...)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list groups.")
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, name, source string
			var members int
			if rows.Scan(&id, &name, &source, &members) == nil {
				items = append(items, map[string]any{"id": id, "name": name, "source": source, "members": members})
			}
		}
		next := ""
		if len(items) > limit {
			last := items[limit-1]
			items = items[:limit]
			next = encodeListCursor(last["name"].(string), last["id"].(string))
		}
		writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "next_cursor": next})
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		writeProblem(w, r, 400, "Invalid group", "A group name is required (maximum 100 characters).")
		return
	}
	id := db.NewID()
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to start group update.")
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO groups(id,name) VALUES($1,$2)`, id, input.Name); err != nil {
		if isUniqueViolation(err) {
			writeProblem(w, r, 409, "Group exists", "A group with this name already exists.")
		} else {
			writeProblem(w, r, 500, "Database error", "Unable to create group.")
		}
		return
	}
	seenMembers := make(map[string]struct{}, len(input.Members))
	for _, member := range input.Members {
		if _, seen := seenMembers[member]; seen {
			continue
		}
		seenMembers[member] = struct{}{}
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO group_members(group_id,user_id) VALUES($1,$2)`, id, member); err != nil {
			if isForeignKeyViolation(err) {
				writeProblem(w, r, 400, "Invalid member", "A member ID is invalid.")
			} else {
				writeProblem(w, r, 500, "Database error", "Unable to add the group member.")
			}
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to save group.")
		return
	}
	s.audit(r.Context(), p.ID, "group.create", "group", id, map[string]any{"name": input.Name, "member_count": len(input.Members)})
	writeJSON(w, 201, map[string]string{"id": id, "name": input.Name})
}

func (s *Server) apiGroup(w http.ResponseWriter, r *http.Request, id string) {
	if (r.Method != http.MethodPatch && r.Method != http.MethodGet) || id == "" || strings.Contains(id, "/") {
		methodNotAllowed(w)
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		var name, source string
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT name,source FROM groups WHERE id=$1`, id).Scan(&name, &source); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeProblem(w, r, http.StatusNotFound, "Group not found", "The group does not exist.")
			} else {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the group.")
			}
			return
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT u.id,u.username,u.role FROM group_members m JOIN users u ON u.id=m.user_id WHERE m.group_id=$1 ORDER BY u.username`, id)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list group members.")
			return
		}
		defer rows.Close()
		members := []map[string]string{}
		for rows.Next() {
			var memberID, username, role string
			if rows.Scan(&memberID, &username, &role) == nil {
				members = append(members, map[string]string{"id": memberID, "username": username, "role": role})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "source": source, "members": members})
		return
	}
	var input struct {
		Name    *string   `json:"name"`
		Members *[]string `json:"members"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Name == nil && input.Members == nil {
		writeProblem(w, r, http.StatusBadRequest, "Empty update", "Set name or members.")
		return
	}
	name := ""
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if name == "" || len(name) > 100 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid group", "A group name is required (maximum 100 characters).")
			return
		}
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to start group update.")
		return
	}
	defer tx.Rollback()
	var currentName string
	if err := tx.QueryRowContext(r.Context(), `SELECT name FROM groups WHERE id=$1 FOR UPDATE`, id).Scan(&currentName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, r, http.StatusNotFound, "Group not found", "The group does not exist.")
		} else {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the group.")
		}
		return
	}
	if input.Name != nil {
		if _, err := tx.ExecContext(r.Context(), `UPDATE groups SET name=$2 WHERE id=$1`, id, name); err != nil {
			if isUniqueViolation(err) {
				writeProblem(w, r, http.StatusConflict, "Group exists", "A group with this name already exists.")
			} else {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to update the group.")
			}
			return
		}
		currentName = name
	}
	if input.Members != nil {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM group_members WHERE group_id=$1`, id); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to replace group members.")
			return
		}
		for _, memberID := range *input.Members {
			if _, err := tx.ExecContext(r.Context(), `INSERT INTO group_members(group_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, memberID); err != nil {
				if isForeignKeyViolation(err) {
					writeProblem(w, r, http.StatusBadRequest, "Invalid member", "A member ID is invalid.")
				} else {
					writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to add the group member.")
				}
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to save group changes.")
		return
	}
	var members int
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM group_members WHERE group_id=$1`, id).Scan(&members)
	s.audit(r.Context(), p.ID, "group.update", "group", id, map[string]any{"name": currentName, "members": members})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": currentName, "members": members})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	args := []any{limit + 1}
	where := ""
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The audit cursor is invalid.")
			return
		}
		parts := strings.SplitN(string(decoded), "|", 2)
		if len(parts) != 2 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The audit cursor is invalid.")
			return
		}
		created, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil || parts[1] == "" {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The audit cursor is invalid.")
			return
		}
		where = "WHERE (e.created_at,e.id) < ($1,$2)"
		args = []any{created, parts[1], limit + 1}
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT e.id,COALESCE(u.username,''),e.action,e.target_type,e.target_id,e.metadata_json,e.request_id,e.created_at FROM audit_events e LEFT JOIN users u ON u.id=e.actor_user_id `+where+` ORDER BY e.created_at DESC,e.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read audit events.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, actor, action, targetType, targetID, metadata, requestID string
		var created time.Time
		if rows.Scan(&id, &actor, &action, &targetType, &targetID, &metadata, &requestID, &created) == nil {
			var decoded any
			_ = json.Unmarshal([]byte(metadata), &decoded)
			items = append(items, map[string]any{"id": id, "actor": actor, "action": action, "target_type": targetType, "target_id": targetID, "metadata": decoded, "request_id": requestID, "created_at": created})
		}
	}
	nextCursor := ""
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		if created, ok := last["created_at"].(time.Time); ok {
			nextCursor = base64.RawURLEncoding.EncodeToString([]byte(created.UTC().Format(time.RFC3339Nano) + "|" + last["id"].(string)))
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": cursor, "next_cursor": nextCursor})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, r, 400, "Invalid JSON", "The request body is invalid or too large.")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeProblem(w, r, 400, "Invalid JSON", "Provide exactly one JSON value.")
		return false
	}
	return true
}

func pageLimit(value string) int {
	if value == "" {
		return 50
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func encodeListCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "|")))
}

func decodeListCursor(value string, count int) ([]string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != count {
		return nil, false
	}
	for _, part := range parts {
		if part == "" || strings.ContainsAny(part, "\r\n") {
			return nil, false
		}
	}
	return parts, true
}

func escapeLikePattern(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func hasScope(p *Principal, scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
