package config

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/logging"
)

// MaxCacheFileSize is the largest file the cache can store: an S3 multipart
// upload takes at most 10000 parts and storage.S3.Put sends 64 MiB parts.
const MaxCacheFileSize = 10000 * (64 << 20)

// DefaultUploadMaxFileSize is the largest published file when none is set.
const DefaultUploadMaxFileSize = 50 << 30

// MaxUploadFileSize is the largest file a user may publish.
func (c Config) MaxUploadFileSize() int64 {
	if c.UploadMaxFileSize <= 0 {
		return DefaultUploadMaxFileSize
	}
	return c.UploadMaxFileSize
}

type Config struct {
	ListenAddr     string
	PublicURL      string
	TrustedProxies []netip.Prefix
	LogLevel       string
	// SetupToken is the code asked for to create the first administrator from
	// the console. Empty means a random one is generated and printed at start-up.
	SetupToken     string
	DatabaseURL    string
	AutoMigrate    bool
	HFBaseURL      string
	HFProxyURL     string
	HFToken        string
	HFAllowedHosts []string
	// HFAllowedPrivateNets lists private networks the service may call (a Hub
	// mirror inside the company network). Empty by default: only public
	// addresses are reachable. Set by the environment only, never from the console.
	HFAllowedPrivateNets   []netip.Prefix
	HFTimeout              time.Duration
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3Prefix               string
	S3ForcePathStyle       bool
	S3AccessKey            string
	S3SecretKey            string
	S3ServerSideEncryption bool
	AuthMode               string
	AnonymousPublicRead    bool
	SessionSecret          string
	// PreviousSessionSecrets are older secrets still accepted while rotating
	// (HFPROXY_SESSION_SECRET_PREVIOUS). New data is always protected with
	// SessionSecret.
	PreviousSessionSecrets []string
	SessionIdleTTL         time.Duration
	SessionAbsoluteTTL     time.Duration
	CacheMaxFileSize       int64
	CacheRefTTL            time.Duration
	CacheNegativeTTL       time.Duration
	CacheGCGrace           time.Duration
	OIDCIssuer             string
	OIDCClientID           string
	OIDCClientSecret       string
	OIDCRedirectURL        string
	OIDCAutoProvision      bool
	LDAPURL                string
	LDAPStartTLS           bool
	LDAPBindDN             string
	LDAPBindPassword       string
	LDAPBaseDN             string
	LDAPUserFilter         string
	LDAPAutoProvision      bool
	MetricsEnabled         bool
	MetricsPath            string
	TracingEnabled         bool
	TracingServiceName     string
	TracingSamplingRatio   float64
	OTLPEndpoint           string
	CacheInventoryRefresh  time.Duration

	// LockedSettings lists the console-editable settings (by their JSON name,
	// for example "s3_bucket") pinned to the environment: saved values are
	// ignored for them and the console refuses to change them
	// (HFPROXY_LOCKED_SETTINGS, "all" locks every one).
	LockedSettings []string

	// Environment-only settings: they are not editable from the console and
	// are never overridden by saved settings.

	// LDAPGroupSync mirrors the user's directory groups (LDAPGroupFilter, with
	// {dn} replaced by the user's DN, searched under LDAPGroupBaseDN) into
	// hfproxy groups with source "ldap" at each sign-in.
	LDAPGroupSync   bool
	LDAPGroupBaseDN string
	LDAPGroupFilter string
	StagingDir      string
	// StagingMinFreeBytes makes /readyz fail while the staging directory has
	// less free space than this (0 turns the check off).
	StagingMinFreeBytes  int64
	CacheFillConcurrency int
	CacheMaxTotalSize    int64
	// CacheEviction chooses which files leave the cache, and when.
	CacheEviction CacheEviction
	// Download limits; 0 turns each one off.
	// DownloadMaxConcurrent is how many transfers one user (or one anonymous
	// client address) may run at once on this instance.
	DownloadMaxConcurrent int64
	// DownloadRateLimit caps the speed of each transfer, in bytes per second.
	DownloadRateLimit int64
	// DownloadDailyQuota is the default amount of data a user may download in
	// 24 hours, in bytes. A user's own quota overrides it.
	DownloadDailyQuota int64
	// CustomRepos lets users publish their own models and datasets on the proxy.
	// UploadMaxFileSize bounds one published file and UploadUserQuota the bytes a
	// user may keep in repositories they own (0 turns the quota off).
	CustomRepos bool
	// SafeFormatsOnly refuses files that can run code when a client loads them
	// (pickle-based weights such as .bin, .pt or .ckpt) unless a repository is
	// exempted.
	SafeFormatsOnly    bool
	UploadMaxFileSize  int64
	UploadUserQuota    int64
	RetentionDownloads time.Duration
	RetentionAudit     time.Duration
	MetricsAddr        string
	ShutdownGrace      time.Duration
	// SettingsRevision is the saved version these settings were read from (see
	// SavePersisted); it is not itself saved.
	SettingsRevision string

	// UpstreamBreakerThreshold is how many consecutive failures of one
	// upstream host open its circuit (0 disables it); UpstreamBreakerCooldown
	// is how long it stays open before a single probe is allowed.
	UpstreamBreakerThreshold int
	UpstreamBreakerCooldown  time.Duration
	// APICacheMaxBytes bounds the in-memory cache of Hub metadata responses
	// (0 disables it); APICacheMaxStale is how long an expired entry may still
	// be served when the Hub is failing.
	APICacheMaxBytes int64
	APICacheMaxStale time.Duration

	// AuthCacheTTL is how long a successful token or session lookup is reused
	// (HFPROXY_AUTH_CACHE_TTL, 0 turns it off). Revocations made through this
	// instance take effect at once; changes made elsewhere within this time.
	AuthCacheTTL time.Duration
	// DownloadLogQueue is the number of hit events buffered before they are
	// written in batches (HFPROXY_DOWNLOAD_LOG_QUEUE, 0 writes each one in the
	// request).
	DownloadLogQueue int
	// DatabaseMaxConns caps the PostgreSQL pool (HFPROXY_DATABASE_MAX_CONNS, 0 =
	// computed from the fill concurrency). Every running cache fill holds one
	// connection for its whole download (the per-file lock), so the pool must
	// stay larger than the fill concurrency or fills would starve each other.
	DatabaseMaxConns int
}

func Load() (Config, error) {
	if err := validateEnvironmentOverrides(); err != nil {
		return Config{}, err
	}
	var c Config
	var err error
	c.ListenAddr = value("LISTEN_ADDR", "127.0.0.1:8080")
	c.PublicURL = value("PUBLIC_URL", "")
	c.TrustedProxies, err = parsePrefixes(split(value("TRUSTED_PROXIES", "")))
	if err != nil {
		return Config{}, fmt.Errorf("HFPROXY_TRUSTED_PROXIES: %w", err)
	}
	c.LogLevel = strings.ToLower(value("LOG_LEVEL", "info"))
	if canonical, err := logging.Normalize(c.LogLevel); err == nil {
		c.LogLevel = canonical // "warning" is accepted for "warn"
	}
	c.DatabaseURL = secret("DATABASE_URL", "")
	c.AutoMigrate = boolean("AUTO_MIGRATE", false)
	c.HFBaseURL = value("HF_BASE_URL", "https://huggingface.co")
	c.HFProxyURL = secret("HF_PROXY_URL", "")
	c.HFToken = secret("HF_TOKEN", "")
	c.HFAllowedHosts, err = NormalizeAllowedHosts(split(value("HF_ALLOWED_REDIRECT_HOSTS", "huggingface.co,hf.co,*.huggingface.co,*.hf.co,*.xethub.hf.co")))
	if err != nil {
		return Config{}, err
	}
	c.HFAllowedPrivateNets, err = ParsePrivateNets(split(value("HF_ALLOWED_PRIVATE_CIDRS", "")))
	if err != nil {
		return Config{}, err
	}
	c.HFTimeout = duration("HF_TIMEOUT", 2*time.Minute)
	c.S3Endpoint = value("S3_ENDPOINT", "")
	c.S3Region = value("S3_REGION", "us-east-1")
	c.S3Bucket = value("S3_BUCKET", "")
	c.S3Prefix = strings.Trim(value("S3_PREFIX", "hfproxy"), "/")
	c.S3ForcePathStyle = boolean("S3_FORCE_PATH_STYLE", true)
	c.S3AccessKey = secret("S3_ACCESS_KEY", "")
	c.S3SecretKey = secret("S3_SECRET_KEY", "")
	c.S3ServerSideEncryption = boolean("S3_SERVER_SIDE_ENCRYPTION", true)
	c.AuthMode = strings.ToLower(value("AUTH_MODE", "local"))
	c.AnonymousPublicRead = boolean("ANONYMOUS_PUBLIC_READ", false)
	c.SetupToken = secret("SETUP_TOKEN", "")
	c.SessionSecret = secret("SESSION_SECRET", "")
	c.PreviousSessionSecrets = splitSecrets(secret("SESSION_SECRET_PREVIOUS", ""))
	c.SessionIdleTTL = duration("SESSION_IDLE_TTL", 8*time.Hour)
	c.SessionAbsoluteTTL = duration("SESSION_ABSOLUTE_TTL", 24*time.Hour)
	c.CacheMaxFileSize = int64(number("CACHE_MAX_FILE_SIZE", 100<<30))
	c.CacheRefTTL = duration("CACHE_REF_TTL", time.Minute)
	c.CacheNegativeTTL = duration("CACHE_NEGATIVE_TTL", 30*time.Second)
	c.CacheGCGrace = duration("CACHE_GC_GRACE", 24*time.Hour)
	c.OIDCIssuer = value("OIDC_ISSUER", "")
	c.OIDCClientID = value("OIDC_CLIENT_ID", "")
	c.OIDCClientSecret = secret("OIDC_CLIENT_SECRET", "")
	c.OIDCRedirectURL = value("OIDC_REDIRECT_URL", "")
	c.OIDCAutoProvision = boolean("OIDC_AUTO_PROVISION", false)
	c.LDAPURL = value("LDAP_URL", "")
	c.LDAPStartTLS = boolean("LDAP_STARTTLS", true)
	c.LDAPBindDN = value("LDAP_BIND_DN", "")
	c.LDAPBindPassword = secret("LDAP_BIND_PASSWORD", "")
	c.LDAPBaseDN = value("LDAP_BASE_DN", "")
	c.LDAPUserFilter = value("LDAP_USER_FILTER", "(uid={username})")
	c.LDAPAutoProvision = boolean("LDAP_AUTO_PROVISION", false)
	c.LDAPGroupSync = boolean("LDAP_GROUP_SYNC", false)
	c.LDAPGroupBaseDN = value("LDAP_GROUP_BASE_DN", c.LDAPBaseDN)
	c.LDAPGroupFilter = value("LDAP_GROUP_FILTER", "(member={dn})")
	c.MetricsEnabled = boolean("METRICS_ENABLED", false)
	c.MetricsPath = value("METRICS_PATH", "/metrics")
	c.TracingEnabled = boolean("TRACING_ENABLED", false)
	c.TracingServiceName = value("TRACING_SERVICE_NAME", "hfproxy")
	c.TracingSamplingRatio, err = strconv.ParseFloat(value("TRACING_SAMPLING_RATIO", "0.1"), 64)
	if err != nil {
		return Config{}, fmt.Errorf("HFPROXY_TRACING_SAMPLING_RATIO must be a number between 0 and 1")
	}
	c.OTLPEndpoint = value("OTLP_ENDPOINT", "")
	c.CacheInventoryRefresh = duration("CACHE_INVENTORY_REFRESH", 5*time.Minute)
	c.LockedSettings = splitSecrets(value("LOCKED_SETTINGS", ""))
	if err := ValidateLockedSettings(c.LockedSettings); err != nil {
		return Config{}, err
	}
	c.StagingDir = value("STAGING_DIR", "")
	c.StagingMinFreeBytes = int64(number("STAGING_MIN_FREE_BYTES", 0))
	c.CacheFillConcurrency = number("CACHE_FILL_CONCURRENCY", 4)
	c.CacheMaxTotalSize = int64(number("CACHE_MAX_TOTAL_SIZE", 0))
	c.CacheEviction, err = cacheEvictionFromEnvironment()
	if err != nil {
		return Config{}, err
	}
	c.DownloadMaxConcurrent = int64(number("DOWNLOAD_MAX_CONCURRENT_PER_USER", 0))
	c.DownloadRateLimit = int64(number("DOWNLOAD_RATE_LIMIT", 0))
	c.DownloadDailyQuota = int64(number("DOWNLOAD_DAILY_QUOTA", 0))
	c.CustomRepos = boolean("CUSTOM_REPOSITORIES", true)
	c.SafeFormatsOnly = boolean("SAFE_FORMATS_ONLY", false)
	c.UploadMaxFileSize = int64(number("UPLOAD_MAX_FILE_SIZE", DefaultUploadMaxFileSize))
	c.UploadUserQuota = int64(number("UPLOAD_USER_QUOTA", 0))
	c.RetentionDownloads = duration("RETENTION_DOWNLOADS", 90*24*time.Hour)
	c.RetentionAudit = duration("RETENTION_AUDIT", 365*24*time.Hour)
	c.MetricsAddr = value("METRICS_ADDR", "")
	c.ShutdownGrace = duration("SHUTDOWN_GRACE", 30*time.Second)
	c.UpstreamBreakerThreshold = number("UPSTREAM_BREAKER_THRESHOLD", 5)
	c.UpstreamBreakerCooldown = duration("UPSTREAM_BREAKER_COOLDOWN", 30*time.Second)
	c.APICacheMaxBytes = int64(number("API_CACHE_MAX_BYTES", 32<<20))
	c.APICacheMaxStale = duration("API_CACHE_MAX_STALE", 24*time.Hour)
	c.AuthCacheTTL = duration("AUTH_CACHE_TTL", 5*time.Second)
	c.DownloadLogQueue = number("DOWNLOAD_LOG_QUEUE", 1024)
	c.DatabaseMaxConns = number("DATABASE_MAX_CONNS", 0)
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// knownDevSecrets are the placeholder session secrets shipped in examples.
var knownDevSecrets = []string{
	"dev-only-session-secret-change-before-deployment-32-bytes",
}

// knownDevS3Keys are the Garage credentials shipped in docker-compose.yml.
var knownDevS3Keys = []string{
	"GK0123456789abcdef0123456789abcdef",
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

// CheckDeployment rejects settings that are acceptable on a developer machine
// but unsafe on a shared deployment. It is applied when the server starts, not
// by Validate, so tools such as `config validate` and tests stay usable.
func (c Config) CheckDeployment() error {
	loopbackPublic := false
	if c.PublicURL != "" {
		if public, err := url.Parse(c.PublicURL); err == nil {
			loopbackPublic = IsLoopbackHost(public.Hostname())
		}
	}
	for _, dev := range knownDevSecrets {
		if c.SessionSecret == dev && !loopbackPublic {
			return fmt.Errorf("HFPROXY_SESSION_SECRET is the published development value; generate a unique secret")
		}
	}
	if !loopbackPublic {
		if strings.Contains(c.DatabaseURL, "hfproxy_dev_password") {
			return fmt.Errorf("HFPROXY_DATABASE_URL uses the published development password; set a unique one")
		}
		for _, dev := range knownDevS3Keys {
			if c.S3AccessKey == dev || c.S3SecretKey == dev {
				return fmt.Errorf("the S3 credentials are the published development keys; generate unique ones")
			}
		}
	}
	if c.PublicURL == "" && !listensOnLoopback(c.ListenAddr) {
		return fmt.Errorf("HFPROXY_PUBLIC_URL is required when listening on a non-loopback address (it enables Secure cookies and HSTS)")
	}
	return nil
}

const maxPreviousSecrets = 4

// splitSecrets splits a list of secrets separated by commas or whitespace.
func splitSecrets(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
}

func listensOnLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return host != "" && IsLoopbackHost(host)
}

func (c Config) Validate() error {
	if c.MetricsPath != "" && (c.MetricsPath[0] != '/' || c.MetricsPath == "/" || strings.Count(c.MetricsPath, "/") != 1 || strings.ContainsAny(c.MetricsPath, "?#")) {
		return fmt.Errorf("HFPROXY_METRICS_PATH must be a non-root top-level URL path")
	}
	if c.MetricsPath == "/healthz" || c.MetricsPath == "/readyz" || c.MetricsPath == "/login" || c.MetricsPath == "/logout" {
		return fmt.Errorf("HFPROXY_METRICS_PATH must not replace a health or authentication endpoint")
	}
	if (c.TracingEnabled && c.TracingServiceName == "") || math.IsNaN(c.TracingSamplingRatio) || math.IsInf(c.TracingSamplingRatio, 0) || c.TracingSamplingRatio < 0 || c.TracingSamplingRatio > 1 {
		return fmt.Errorf("HFPROXY_TRACING_SERVICE_NAME must be non-empty and HFPROXY_TRACING_SAMPLING_RATIO must be between 0 and 1")
	}
	if c.OTLPEndpoint != "" {
		endpoint, err := url.Parse(c.OTLPEndpoint)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return fmt.Errorf("HFPROXY_OTLP_ENDPOINT must be an HTTP(S) OTLP endpoint URL without credentials or query parameters")
		}
	}
	if c.TracingEnabled && c.OTLPEndpoint == "" {
		return fmt.Errorf("HFPROXY_OTLP_ENDPOINT is required when tracing is enabled")
	}
	if c.StagingMinFreeBytes < 0 {
		return fmt.Errorf("HFPROXY_STAGING_MIN_FREE_BYTES must not be negative")
	}
	if c.UploadMaxFileSize < 0 || c.UploadMaxFileSize > MaxCacheFileSize || c.UploadUserQuota < 0 {
		return fmt.Errorf("HFPROXY_UPLOAD_MAX_FILE_SIZE must be at most %d bytes (0 means 50 GiB), and HFPROXY_UPLOAD_USER_QUOTA must not be negative", MaxCacheFileSize)
	}
	if c.DownloadMaxConcurrent < 0 || c.DownloadRateLimit < 0 || c.DownloadDailyQuota < 0 {
		return fmt.Errorf("the download limits (HFPROXY_DOWNLOAD_*) must not be negative")
	}
	if c.CacheFillConcurrency < 0 || c.CacheMaxTotalSize < 0 || c.RetentionDownloads < 0 || c.RetentionAudit < 0 {
		return fmt.Errorf("HFPROXY_CACHE_FILL_CONCURRENCY, HFPROXY_CACHE_MAX_TOTAL_SIZE and the retention periods must not be negative")
	}
	if err := c.CacheEviction.Validate(); err != nil {
		return err
	}
	if c.AuthCacheTTL < 0 || c.AuthCacheTTL > time.Minute || c.DownloadLogQueue < 0 {
		return fmt.Errorf("HFPROXY_AUTH_CACHE_TTL must be between 0 and 1m and HFPROXY_DOWNLOAD_LOG_QUEUE must not be negative")
	}
	if c.DatabaseMaxConns < 0 || (c.DatabaseMaxConns > 0 && c.DatabaseMaxConns < c.CacheFillConcurrency+8) {
		return fmt.Errorf("HFPROXY_DATABASE_MAX_CONNS must be at least HFPROXY_CACHE_FILL_CONCURRENCY plus 8: each running fill holds a connection")
	}
	if c.UpstreamBreakerThreshold < 0 || c.UpstreamBreakerCooldown < 0 || c.APICacheMaxBytes < 0 || c.APICacheMaxStale < 0 {
		return fmt.Errorf("the upstream breaker and API cache settings must not be negative")
	}
	if c.UpstreamBreakerThreshold > 0 && c.UpstreamBreakerCooldown == 0 {
		return fmt.Errorf("HFPROXY_UPSTREAM_BREAKER_COOLDOWN must be positive when the breaker is enabled")
	}
	if c.ShutdownGrace < 0 {
		return fmt.Errorf("HFPROXY_SHUTDOWN_GRACE must not be negative")
	}
	if c.MetricsAddr != "" {
		if _, _, err := net.SplitHostPort(c.MetricsAddr); err != nil {
			return fmt.Errorf("HFPROXY_METRICS_ADDR must be host:port")
		}
	}
	if c.CacheInventoryRefresh < 0 {
		return fmt.Errorf("HFPROXY_CACHE_INVENTORY_REFRESH must not be negative")
	}
	if _, err := logging.Normalize(c.LogLevel); err != nil || c.LogLevel == "" {
		return fmt.Errorf("HFPROXY_LOG_LEVEL must be debug, info, warn (or warning), or error")
	}
	if c.PublicURL != "" {
		public, publicErr := url.Parse(c.PublicURL)
		if publicErr != nil || public.Host == "" || public.User != nil || public.RawQuery != "" || public.Fragment != "" || (public.Scheme != "https" && public.Scheme != "http") {
			return fmt.Errorf("HFPROXY_PUBLIC_URL must be an HTTP(S) URL without credentials, query, or fragment")
		}
		if public.Scheme != "https" && !IsLoopbackHost(public.Hostname()) {
			return fmt.Errorf("HFPROXY_PUBLIC_URL must use HTTPS outside loopback development")
		}
	}
	base, err := url.Parse(c.HFBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("HFPROXY_HF_BASE_URL must be an HTTP(S) URL without user information")
	}
	if base.Scheme != "https" && !IsLoopbackHost(base.Hostname()) {
		return fmt.Errorf("HFPROXY_HF_BASE_URL must use HTTPS outside loopback development")
	}
	if c.HFProxyURL != "" {
		proxy, proxyErr := url.Parse(c.HFProxyURL)
		if proxyErr != nil || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") {
			return fmt.Errorf("HFPROXY_HF_PROXY_URL must be a proxy URL without a path, query, or fragment")
		}
		proxyScheme := strings.ToLower(proxy.Scheme)
		if proxyScheme != "http" && proxyScheme != "https" && proxyScheme != "socks4" && proxyScheme != "socks4a" && proxyScheme != "socks5" && proxyScheme != "socks5h" {
			return fmt.Errorf("HFPROXY_HF_PROXY_URL scheme must be http, https, socks4, socks4a, socks5, or socks5h")
		}
		if port := proxy.Port(); port != "" {
			parsedPort, portErr := strconv.Atoi(port)
			if portErr != nil || parsedPort < 1 || parsedPort > 65535 {
				return fmt.Errorf("HFPROXY_HF_PROXY_URL contains an invalid port")
			}
		}
		if proxy.User != nil {
			username := proxy.User.Username()
			password, hasPassword := proxy.User.Password()
			if strings.HasPrefix(proxyScheme, "socks4") {
				if hasPassword || len(username) > 255 || strings.ContainsRune(username, '\x00') {
					return fmt.Errorf("SOCKS4 supports a user ID only, up to 255 bytes")
				}
			}
			if strings.HasPrefix(proxyScheme, "socks5") && (username == "" || len(username) > 255 || len(password) > 255) {
				return fmt.Errorf("SOCKS5 username and password must each be at most 255 bytes")
			}
		}
	}
	if c.HFTimeout <= 0 || c.CacheMaxFileSize <= 0 {
		return fmt.Errorf("HFPROXY_HF_TIMEOUT and HFPROXY_CACHE_MAX_FILE_SIZE must be positive")
	}
	if c.CacheMaxFileSize > MaxCacheFileSize {
		return fmt.Errorf("HFPROXY_CACHE_MAX_FILE_SIZE cannot exceed %d bytes (625 GiB): object storage accepts at most 10000 parts of 64 MiB per file, so a larger limit would only fail after hours of transfer", MaxCacheFileSize)
	}
	if c.CacheRefTTL <= 0 || c.CacheNegativeTTL < 0 || c.CacheGCGrace < 0 {
		return fmt.Errorf("cache TTL and garbage collection grace values are invalid")
	}
	if c.AuthMode != "local" && c.AuthMode != "oidc" {
		return fmt.Errorf("HFPROXY_AUTH_MODE must be local or oidc")
	}
	if c.AuthMode == "oidc" || c.OIDCIssuer != "" {
		if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.OIDCRedirectURL == "" {
			return fmt.Errorf("OIDC requires HFPROXY_OIDC_ISSUER, HFPROXY_OIDC_CLIENT_ID, HFPROXY_OIDC_CLIENT_SECRET, and HFPROXY_OIDC_REDIRECT_URL")
		}
		issuer, issuerErr := url.Parse(c.OIDCIssuer)
		if issuerErr != nil || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" || (issuer.Scheme != "https" && !(issuer.Scheme == "http" && IsLoopbackHost(issuer.Hostname()))) {
			return fmt.Errorf("HFPROXY_OIDC_ISSUER must use HTTPS outside loopback development")
		}
		redirect, parseErr := url.Parse(c.OIDCRedirectURL)
		if parseErr != nil || redirect.Host == "" || (redirect.Scheme != "https" && !IsLoopbackHost(redirect.Hostname())) {
			return fmt.Errorf("HFPROXY_OIDC_REDIRECT_URL must use HTTPS outside loopback development")
		}
	}
	if c.SessionSecret == "" {
		return fmt.Errorf("HFPROXY_SESSION_SECRET is required; use a mounted secret or *_FILE")
	}
	if c.LDAPURL != "" {
		ldapURL, ldapErr := url.Parse(c.LDAPURL)
		if ldapErr != nil || ldapURL.Host == "" || ldapURL.User != nil || (ldapURL.Scheme != "ldaps" && ldapURL.Scheme != "ldap") || c.LDAPBaseDN == "" || !strings.Contains(c.LDAPUserFilter, "{username}") {
			return fmt.Errorf("LDAP requires a valid HFPROXY_LDAP_URL, HFPROXY_LDAP_BASE_DN, and a HFPROXY_LDAP_USER_FILTER containing {username}")
		}
		if c.LDAPGroupSync && (c.LDAPGroupBaseDN == "" || !strings.Contains(c.LDAPGroupFilter, "{dn}")) {
			return fmt.Errorf("HFPROXY_LDAP_GROUP_SYNC requires HFPROXY_LDAP_GROUP_BASE_DN and a HFPROXY_LDAP_GROUP_FILTER containing {dn}")
		}
		if ldapURL.Scheme == "ldap" && !c.LDAPStartTLS {
			return fmt.Errorf("plain LDAP is disabled; use ldaps:// or enable StartTLS")
		}
		if c.LDAPBindDN != "" && c.LDAPBindPassword == "" {
			return fmt.Errorf("HFPROXY_LDAP_BIND_PASSWORD is required when a bind DN is configured")
		}
	}
	if c.SetupToken != "" && (len(c.SetupToken) < 16 || len(c.SetupToken) > 256) {
		return fmt.Errorf("HFPROXY_SETUP_TOKEN must contain 16 to 256 characters")
	}
	if c.SessionSecret != "" && len(c.SessionSecret) < 32 {
		return fmt.Errorf("HFPROXY_SESSION_SECRET must contain at least 32 bytes")
	}
	if len(c.PreviousSessionSecrets) > maxPreviousSecrets {
		return fmt.Errorf("HFPROXY_SESSION_SECRET_PREVIOUS lists %d secrets; keep at most %d", len(c.PreviousSessionSecrets), maxPreviousSecrets)
	}
	for _, previous := range c.PreviousSessionSecrets {
		if len(previous) < 32 {
			return fmt.Errorf("every secret in HFPROXY_SESSION_SECRET_PREVIOUS must contain at least 32 bytes")
		}
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("HFPROXY_DATABASE_URL is required")
	}
	if c.S3Endpoint != "" {
		endpoint, endpointErr := url.Parse(c.S3Endpoint)
		if endpointErr != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return fmt.Errorf("HFPROXY_S3_ENDPOINT must be an HTTP(S) URL without credentials or query parameters")
		}
	}
	if (c.S3AccessKey == "") != (c.S3SecretKey == "") {
		return fmt.Errorf("HFPROXY_S3_ACCESS_KEY and HFPROXY_S3_SECRET_KEY must be set together")
	}
	if c.SessionIdleTTL <= 0 || c.SessionAbsoluteTTL < c.SessionIdleTTL {
		return fmt.Errorf("session TTLs must be positive and the absolute TTL must be at least the idle TTL")
	}
	return nil
}

func value(key, fallback string) string {
	name := "HFPROXY_" + key
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func secret(key, fallback string) string {
	name := "HFPROXY_" + key
	if file := os.Getenv(name + "_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return ""
		}
		return strings.TrimRight(string(data), "\r\n")
	}
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func validateEnvironmentOverrides() error {
	for _, key := range []string{
		"DATABASE_URL", "HF_PROXY_URL", "HF_TOKEN", "S3_ACCESS_KEY", "S3_SECRET_KEY", "SESSION_SECRET", "SESSION_SECRET_PREVIOUS",
		"OIDC_CLIENT_SECRET", "LDAP_BIND_PASSWORD", "SETUP_TOKEN",
	} {
		if file := os.Getenv("HFPROXY_" + key + "_FILE"); file != "" {
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("HFPROXY_%s_FILE cannot be read: %w", key, err)
			}
			if strings.TrimSpace(string(data)) == "" {
				return fmt.Errorf("HFPROXY_%s_FILE must not be empty", key)
			}
		}
	}
	for _, key := range []string{"AUTO_MIGRATE", "S3_FORCE_PATH_STYLE", "S3_SERVER_SIDE_ENCRYPTION", "ANONYMOUS_PUBLIC_READ", "OIDC_AUTO_PROVISION", "LDAP_STARTTLS", "LDAP_AUTO_PROVISION"} {
		name := "HFPROXY_" + key
		if value, ok := os.LookupEnv(name); ok && value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("%s must be a boolean", name)
			}
		}
	}
	for _, key := range []string{"HF_TIMEOUT", "SESSION_IDLE_TTL", "SESSION_ABSOLUTE_TTL", "CACHE_REF_TTL", "CACHE_NEGATIVE_TTL", "CACHE_GC_GRACE", "RETENTION_DOWNLOADS", "RETENTION_AUDIT", "UPSTREAM_BREAKER_COOLDOWN", "API_CACHE_MAX_STALE"} {
		name := "HFPROXY_" + key
		if value, ok := os.LookupEnv(name); ok && value != "" {
			if _, err := time.ParseDuration(value); err != nil {
				return fmt.Errorf("%s must be a valid Go duration", name)
			}
		}
	}
	for _, key := range []string{"CACHE_MAX_FILE_SIZE", "CACHE_MAX_TOTAL_SIZE", "DOWNLOAD_MAX_CONCURRENT_PER_USER", "DOWNLOAD_RATE_LIMIT", "DOWNLOAD_DAILY_QUOTA", "UPLOAD_MAX_FILE_SIZE", "UPLOAD_USER_QUOTA", "CACHE_FILL_CONCURRENCY", "UPSTREAM_BREAKER_THRESHOLD", "API_CACHE_MAX_BYTES", "STAGING_MIN_FREE_BYTES"} {
		name := "HFPROXY_" + key
		if value, ok := os.LookupEnv(name); ok && value != "" {
			if _, err := strconv.Atoi(value); err != nil {
				return fmt.Errorf("%s must be an integer", name)
			}
		}
	}
	return nil
}

func boolean(key string, fallback bool) bool {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return fallback
	}
	v, err := strconv.ParseBool(text)
	if err != nil {
		return fallback
	}
	return v
}

func number(key string, fallback int) int {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return fallback
	}
	v, err := strconv.Atoi(text)
	if err != nil {
		return fallback
	}
	return v
}

func duration(key string, fallback time.Duration) time.Duration {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return fallback
	}
	v, err := time.ParseDuration(text)
	if err != nil {
		return fallback
	}
	return v
}

func split(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if host := strings.TrimSpace(strings.ToLower(part)); host != "" {
			result = append(result, host)
		}
	}
	return result
}

func parsePrefixes(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", value)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// privateRanges are the only networks HFPROXY_HF_ALLOWED_PRIVATE_CIDRS may
// name: RFC 1918 and unique-local addresses, carrier-grade NAT (used by many
// clouds and clusters) and the benchmarking range. Anything public is reachable
// anyway, and link-local addresses (instance metadata) can never be allowed.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("fc00::/7"),
}

// ParsePrivateNets parses a list of CIDRs, each of which must lie entirely
// inside a private range.
func ParsePrivateNets(values []string) ([]netip.Prefix, error) {
	var nets []netip.Prefix
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("HFPROXY_HF_ALLOWED_PRIVATE_CIDRS: %q is not a CIDR such as 10.20.0.0/16", value)
		}
		prefix = prefix.Masked()
		inside := false
		for _, private := range privateRanges {
			if private.Bits() <= prefix.Bits() && private.Contains(prefix.Addr()) {
				inside = true
			}
		}
		if !inside {
			return nil, fmt.Errorf("HFPROXY_HF_ALLOWED_PRIVATE_CIDRS: %s is not inside a private range (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 198.18.0.0/15, fc00::/7)", prefix)
		}
		nets = append(nets, prefix)
	}
	return nets, nil
}

func NormalizeAllowedHosts(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, raw := range values {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if host == "" {
			continue
		}
		if strings.HasPrefix(host, "*.") {
			suffix := strings.TrimPrefix(host, "*.")
			if suffix == "" || !strings.Contains(suffix, ".") || strings.ContainsAny(suffix, "*/\\") {
				return nil, fmt.Errorf("invalid wildcard redirect host %q", raw)
			}
		} else if strings.ContainsAny(host, "*/\\") {
			return nil, fmt.Errorf("invalid redirect host %q", raw)
		}
		result = append(result, host)
	}
	return result, nil
}

// IsLoopbackHost reports whether host is localhost or a loopback IP address.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsLoopback()
	}
	return false
}

// PoolSize is the number of PostgreSQL connections to allow: the configured
// value, or enough for every fill slot plus the requests around them.
func (c Config) PoolSize() int {
	if c.DatabaseMaxConns > 0 {
		return c.DatabaseMaxConns
	}
	fills := c.CacheFillConcurrency
	if fills <= 0 {
		fills = 4
	}
	if size := 2*fills + 16; size > 20 {
		return size
	}
	return 20
}
