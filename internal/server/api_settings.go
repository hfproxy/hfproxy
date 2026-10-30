package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/logging"
	"github.com/hfproxy/hfproxy/internal/storage"
)

type settingsResponse struct {
	PublicURL              string               `json:"public_url"`
	HFBaseURL              string               `json:"hf_base_url"`
	HFProxySet             bool                 `json:"hf_proxy_set"`
	HFAllowedHosts         []string             `json:"hf_allowed_hosts"`
	HFTimeout              string               `json:"hf_timeout"`
	HFTokenSet             bool                 `json:"hf_token_set"`
	S3Endpoint             string               `json:"s3_endpoint"`
	S3Region               string               `json:"s3_region"`
	S3Bucket               string               `json:"s3_bucket"`
	S3Prefix               string               `json:"s3_prefix"`
	S3ForcePathStyle       bool                 `json:"s3_force_path_style"`
	S3ServerSideEncryption bool                 `json:"s3_server_side_encryption"`
	S3AccessKeySet         bool                 `json:"s3_access_key_set"`
	S3SecretKeySet         bool                 `json:"s3_secret_key_set"`
	AuthMode               string               `json:"auth_mode"`
	AnonymousPublicRead    bool                 `json:"anonymous_public_read"`
	SessionIdleTTL         string               `json:"session_idle_ttl"`
	SessionAbsoluteTTL     string               `json:"session_absolute_ttl"`
	CacheMaxFileSize       int64                `json:"cache_max_file_size"`
	CacheMaxTotalSize      int64                `json:"cache_max_total_size"`
	CacheEviction          config.CacheEviction `json:"cache_eviction"`
	DownloadMaxConcurrent  int64                `json:"download_max_concurrent"`
	DownloadRateLimit      int64                `json:"download_rate_limit"`
	DownloadDailyQuota     int64                `json:"download_daily_quota"`
	CustomRepos            bool                 `json:"custom_repositories"`
	SafeFormatsOnly        bool                 `json:"safe_formats_only"`
	UploadMaxFileSize      int64                `json:"upload_max_file_size"`
	UploadUserQuota        int64                `json:"upload_user_quota"`
	CacheRefTTL            string               `json:"cache_ref_ttl"`
	CacheNegativeTTL       string               `json:"cache_negative_ttl"`
	CacheGCGrace           string               `json:"cache_gc_grace"`
	OIDCIssuer             string               `json:"oidc_issuer"`
	OIDCClientID           string               `json:"oidc_client_id"`
	OIDCClientSecretSet    bool                 `json:"oidc_client_secret_set"`
	OIDCRedirectURL        string               `json:"oidc_redirect_url"`
	OIDCAutoProvision      bool                 `json:"oidc_auto_provision"`
	LDAPURL                string               `json:"ldap_url"`
	LDAPStartTLS           bool                 `json:"ldap_starttls"`
	LDAPBindDN             string               `json:"ldap_bind_dn"`
	LDAPBindPasswordSet    bool                 `json:"ldap_bind_password_set"`
	LDAPBaseDN             string               `json:"ldap_base_dn"`
	LDAPUserFilter         string               `json:"ldap_user_filter"`
	LDAPAutoProvision      bool                 `json:"ldap_auto_provision"`
	MetricsEnabled         bool                 `json:"metrics_enabled"`
	MetricsPath            string               `json:"metrics_path"`
	TracingEnabled         bool                 `json:"tracing_enabled"`
	TracingServiceName     string               `json:"tracing_service_name"`
	TracingSamplingRatio   float64              `json:"tracing_sampling_ratio"`
	OTLPEndpoint           string               `json:"otlp_endpoint"`
	CacheInventoryRefresh  string               `json:"cache_inventory_refresh"`
	LogLevel               string               `json:"log_level"`
	// Revision names the saved version these settings come from; saving sends it
	// back, and is refused when the settings changed in between.
	Revision string `json:"revision"`
	// Locked lists the settings pinned to the environment (read-only here).
	Locked []string `json:"locked"`
}

type settingsInput struct {
	PublicURL              string                `json:"public_url"`
	HFBaseURL              string                `json:"hf_base_url"`
	HFProxyURL             string                `json:"hf_proxy_url"`
	ClearHFProxy           bool                  `json:"clear_hf_proxy"`
	HFAllowedHosts         []string              `json:"hf_allowed_hosts"`
	HFTimeout              string                `json:"hf_timeout"`
	HFToken                string                `json:"hf_token"`
	ClearHFToken           bool                  `json:"clear_hf_token"`
	S3Endpoint             string                `json:"s3_endpoint"`
	S3Region               string                `json:"s3_region"`
	S3Bucket               string                `json:"s3_bucket"`
	S3Prefix               string                `json:"s3_prefix"`
	S3ForcePathStyle       bool                  `json:"s3_force_path_style"`
	S3ServerSideEncryption bool                  `json:"s3_server_side_encryption"`
	S3AccessKey            string                `json:"s3_access_key"`
	S3SecretKey            string                `json:"s3_secret_key"`
	ClearS3Credentials     bool                  `json:"clear_s3_credentials"`
	AuthMode               string                `json:"auth_mode"`
	AnonymousPublicRead    bool                  `json:"anonymous_public_read"`
	SessionIdleTTL         string                `json:"session_idle_ttl"`
	SessionAbsoluteTTL     string                `json:"session_absolute_ttl"`
	CacheMaxFileSize       int64                 `json:"cache_max_file_size"`
	CacheMaxTotalSize      *int64                `json:"cache_max_total_size"`
	CacheEviction          *config.CacheEviction `json:"cache_eviction"`
	DownloadMaxConcurrent  *int64                `json:"download_max_concurrent"`
	DownloadRateLimit      *int64                `json:"download_rate_limit"`
	DownloadDailyQuota     *int64                `json:"download_daily_quota"`
	CustomRepos            *bool                 `json:"custom_repositories"`
	SafeFormatsOnly        *bool                 `json:"safe_formats_only"`
	UploadMaxFileSize      *int64                `json:"upload_max_file_size"`
	UploadUserQuota        *int64                `json:"upload_user_quota"`
	CacheRefTTL            string                `json:"cache_ref_ttl"`
	CacheNegativeTTL       string                `json:"cache_negative_ttl"`
	CacheGCGrace           string                `json:"cache_gc_grace"`
	OIDCIssuer             string                `json:"oidc_issuer"`
	OIDCClientID           string                `json:"oidc_client_id"`
	OIDCClientSecret       string                `json:"oidc_client_secret"`
	ClearOIDCSecret        bool                  `json:"clear_oidc_client_secret"`
	OIDCRedirectURL        string                `json:"oidc_redirect_url"`
	OIDCAutoProvision      bool                  `json:"oidc_auto_provision"`
	LDAPURL                string                `json:"ldap_url"`
	LDAPStartTLS           bool                  `json:"ldap_starttls"`
	LDAPBindDN             string                `json:"ldap_bind_dn"`
	LDAPBindPassword       string                `json:"ldap_bind_password"`
	ClearLDAPPassword      bool                  `json:"clear_ldap_bind_password"`
	LDAPBaseDN             string                `json:"ldap_base_dn"`
	LDAPUserFilter         string                `json:"ldap_user_filter"`
	LDAPAutoProvision      bool                  `json:"ldap_auto_provision"`
	MetricsEnabled         bool                  `json:"metrics_enabled"`
	MetricsPath            string                `json:"metrics_path"`
	TracingEnabled         bool                  `json:"tracing_enabled"`
	TracingServiceName     string                `json:"tracing_service_name"`
	TracingSamplingRatio   float64               `json:"tracing_sampling_ratio"`
	OTLPEndpoint           string                `json:"otlp_endpoint"`
	CacheInventoryRefresh  string                `json:"cache_inventory_refresh"`
	LogLevel               string                `json:"log_level"`
	// Revision is the one read with the settings being edited.
	Revision string `json:"revision"`
	// Locked is part of what the console reads back; it is ignored on input.
	Locked []string `json:"locked"`
}

func settingsView(cfg config.Config) settingsResponse {
	return settingsResponse{
		PublicURL: cfg.PublicURL, HFBaseURL: cfg.HFBaseURL, HFProxySet: cfg.HFProxyURL != "", HFAllowedHosts: cfg.HFAllowedHosts, HFTimeout: cfg.HFTimeout.String(), HFTokenSet: cfg.HFToken != "",
		S3Endpoint: cfg.S3Endpoint, S3Region: cfg.S3Region, S3Bucket: cfg.S3Bucket, S3Prefix: cfg.S3Prefix, S3ForcePathStyle: cfg.S3ForcePathStyle, S3ServerSideEncryption: cfg.S3ServerSideEncryption,
		S3AccessKeySet: cfg.S3AccessKey != "", S3SecretKeySet: cfg.S3SecretKey != "", AuthMode: cfg.AuthMode, AnonymousPublicRead: cfg.AnonymousPublicRead,
		SessionIdleTTL: cfg.SessionIdleTTL.String(), SessionAbsoluteTTL: cfg.SessionAbsoluteTTL.String(), CacheMaxFileSize: cfg.CacheMaxFileSize, CacheMaxTotalSize: cfg.CacheMaxTotalSize, CacheEviction: cfg.CacheEviction.Normalized(), DownloadMaxConcurrent: cfg.DownloadMaxConcurrent, DownloadRateLimit: cfg.DownloadRateLimit, DownloadDailyQuota: cfg.DownloadDailyQuota, CustomRepos: cfg.CustomRepos, SafeFormatsOnly: cfg.SafeFormatsOnly, UploadMaxFileSize: cfg.MaxUploadFileSize(), UploadUserQuota: cfg.UploadUserQuota,
		CacheRefTTL: cfg.CacheRefTTL.String(), CacheNegativeTTL: cfg.CacheNegativeTTL.String(), CacheGCGrace: cfg.CacheGCGrace.String(),
		OIDCIssuer: cfg.OIDCIssuer, OIDCClientID: cfg.OIDCClientID, OIDCClientSecretSet: cfg.OIDCClientSecret != "", OIDCRedirectURL: cfg.OIDCRedirectURL,
		OIDCAutoProvision: cfg.OIDCAutoProvision, LDAPURL: cfg.LDAPURL, LDAPStartTLS: cfg.LDAPStartTLS, LDAPBindDN: cfg.LDAPBindDN,
		LDAPBindPasswordSet: cfg.LDAPBindPassword != "", LDAPBaseDN: cfg.LDAPBaseDN, LDAPUserFilter: cfg.LDAPUserFilter, LDAPAutoProvision: cfg.LDAPAutoProvision,
		MetricsEnabled: cfg.MetricsEnabled, MetricsPath: cfg.MetricsPath, TracingEnabled: cfg.TracingEnabled, TracingServiceName: cfg.TracingServiceName, TracingSamplingRatio: cfg.TracingSamplingRatio, OTLPEndpoint: cfg.OTLPEndpoint, CacheInventoryRefresh: cfg.CacheInventoryRefresh.String(), LogLevel: cfg.LogLevel,
		Revision: cfg.SettingsRevision,
		Locked:   config.LockedNames(cfg.LockedSettings),
	}
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Settings saved from another instance are not seen otherwise.
		if err := s.reloadSavedSettings(r.Context()); err != nil {
			s.logger.Warn("saved settings could not be reloaded", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
		}
		writeJSON(w, http.StatusOK, settingsView(s.currentConfig()))
	case http.MethodPut:
		var input settingsInput
		if !decodeJSON(w, r, &input) {
			return
		}
		current := s.currentConfig()
		if input.Revision != current.SettingsRevision {
			s.settingsChanged(w, r)
			return
		}
		cfg, err := input.apply(current)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid settings", err.Error())
			return
		}
		if changed := config.ChangedLocked(current.LockedSettings, current, cfg); len(changed) > 0 {
			writeProblem(w, r, http.StatusConflict, "Settings locked by the environment", "These settings are set through HFPROXY_* variables and cannot be changed here: "+strings.Join(changed, ", ")+".")
			return
		}
		if err := s.applyConfig(r.Context(), cfg, p.ID); err != nil {
			if errors.Is(err, config.ErrSettingsConflict) {
				s.settingsChanged(w, r)
				return
			}
			s.logger.Warn("settings update failed", "request_id", requestID(r.Context()), "actor_id", p.ID, "error", safeLogValue(err.Error(), 300))
			writeProblem(w, r, http.StatusBadRequest, "Settings could not be applied", "Check provider availability and configuration, then retry.")
			return
		}
		s.logger.Info("settings applied", "request_id", requestID(r.Context()), "actor_id", p.ID, "changed", config.OverriddenFields(current, cfg))
		fields := []string{"public_url", "huggingface", "s3", "authentication", "cache", "observability", "logging"}
		s.audit(r.Context(), p.ID, "settings.update", "settings", "runtime", map[string]any{"updated_sections": fields})
		// The running copy carries the new revision the console must send next.
		writeJSON(w, http.StatusOK, settingsView(s.currentConfig()))
	case http.MethodDelete:
		writeProblem(w, r, http.StatusNotImplemented, "Reset unavailable", "Edit settings directly; environment settings are not reloaded while the process is running.")
	default:
		methodNotAllowed(w)
	}
}

// settingsChanged answers a save made on settings that have changed since the
// console read them. This instance adopts the saved version first, so the
// console's reload shows what is really saved.
func (s *Server) settingsChanged(w http.ResponseWriter, r *http.Request) {
	if err := s.reloadSavedSettings(r.Context()); err != nil {
		s.logger.Warn("saved settings could not be reloaded", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
	}
	writeProblem(w, r, http.StatusConflict, "Settings changed", "The settings were changed by someone else since you opened them. Reload the page to see the current values, then apply your change again.")
}

func (in settingsInput) apply(cfg config.Config) (config.Config, error) {
	var err error
	cfg.MetricsEnabled = in.MetricsEnabled
	cfg.MetricsPath = strings.TrimSpace(in.MetricsPath)
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = "/metrics"
	}
	cfg.TracingEnabled = in.TracingEnabled
	cfg.TracingServiceName = strings.TrimSpace(in.TracingServiceName)
	if cfg.TracingServiceName == "" {
		cfg.TracingServiceName = "hfproxy"
	}
	cfg.TracingSamplingRatio = in.TracingSamplingRatio
	cfg.OTLPEndpoint = strings.TrimSpace(in.OTLPEndpoint)
	cfg.CacheInventoryRefresh, err = time.ParseDuration(in.CacheInventoryRefresh)
	if in.CacheInventoryRefresh == "" {
		cfg.CacheInventoryRefresh = 5 * time.Minute
	} else if err != nil {
		return config.Config{}, fmt.Errorf("cache inventory refresh must be a valid Go duration")
	}
	// Leaving the level out keeps the current one.
	if strings.TrimSpace(in.LogLevel) != "" {
		level, levelErr := logging.Normalize(in.LogLevel)
		if levelErr != nil {
			return config.Config{}, fmt.Errorf("log level must be debug, info, warn, or error")
		}
		cfg.LogLevel = level
	}
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
	// Left out of the request, the limit stays as it is; sending 0 removes it.
	if in.CacheMaxTotalSize != nil {
		if *in.CacheMaxTotalSize < 0 {
			return config.Config{}, errors.New("the total cache size limit cannot be negative")
		}
		cfg.CacheMaxTotalSize = *in.CacheMaxTotalSize
	}
	// Older clients leave it out, and it stays as it is.
	if in.CacheEviction != nil {
		if err := in.CacheEviction.Validate(); err != nil {
			return config.Config{}, err
		}
		cfg.CacheEviction = in.CacheEviction.Normalized()
	}
	// Like the cache limit, a limit left out of the request stays as it is and 0 turns it off.
	for _, limit := range []struct {
		name  string
		input *int64
		field *int64
	}{
		{"concurrent downloads per user", in.DownloadMaxConcurrent, &cfg.DownloadMaxConcurrent},
		{"download speed limit", in.DownloadRateLimit, &cfg.DownloadRateLimit},
		{"daily download quota", in.DownloadDailyQuota, &cfg.DownloadDailyQuota},
		{"upload quota per user", in.UploadUserQuota, &cfg.UploadUserQuota},
	} {
		if limit.input == nil {
			continue
		}
		if *limit.input < 0 {
			return config.Config{}, fmt.Errorf("the %s cannot be negative", limit.name)
		}
		*limit.field = *limit.input
	}
	if in.CustomRepos != nil {
		cfg.CustomRepos = *in.CustomRepos
	}
	if in.SafeFormatsOnly != nil {
		cfg.SafeFormatsOnly = *in.SafeFormatsOnly
	}
	if in.UploadMaxFileSize != nil {
		if *in.UploadMaxFileSize < 0 || *in.UploadMaxFileSize > config.MaxCacheFileSize {
			return config.Config{}, fmt.Errorf("the largest published file cannot be negative or above %d bytes", config.MaxCacheFileSize)
		}
		cfg.UploadMaxFileSize = *in.UploadMaxFileSize
	}
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
		s.fail(w, r, err, http.StatusBadGateway, "S3 connection failed", "Check the endpoint, bucket, region, and credentials.")
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
	if cfg.HFToken != "" && !input.Clear {
		explicitToken := strings.TrimSpace(input.Token) != ""
		if explicitToken || sameURLOrigin(s.currentConfig().HFBaseURL, cfg.HFBaseURL) {
			req.Header.Set("Authorization", "Bearer "+cfg.HFToken)
		}
	}
	resp, err := newHFHTTPClient(cfg).Do(req)
	if err != nil {
		s.fail(w, r, err, http.StatusBadGateway, "Hugging Face connection failed", "Check the Hub URL, proxy scheme and credentials, network access, and token.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && cfg.HFToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "huggingface", "token_configured": false})
		return
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		s.fail(w, r, err, http.StatusBadGateway, "Hugging Face rejected the token", "The Hub is reachable, but the configured token is missing or does not have access.")
		return
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		s.fail(w, r, err, http.StatusBadGateway, "Proxy authentication failed", "Check the configured proxy credentials.")
		return
	}
	if resp.StatusCode >= 500 {
		s.fail(w, r, err, http.StatusBadGateway, "Hugging Face is unavailable", "The Hub returned a server error.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "huggingface"})
}

func sameURLOrigin(first, second string) bool {
	a, errA := url.Parse(first)
	b, errB := url.Parse(second)
	if errA != nil || errB != nil || a.Hostname() == "" || b.Hostname() == "" || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if value := u.Port(); value != "" {
			return value
		}
		switch strings.ToLower(u.Scheme) {
		case "https":
			return "443"
		case "http":
			return "80"
		default:
			return ""
		}
	}
	return port(a) == port(b)
}
