package config

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/keyring"
	"github.com/hfproxy/hfproxy/internal/logging"
)

type managedSettings struct {
	PublicURL              string         `json:"public_url"`
	HFBaseURL              string         `json:"hf_base_url"`
	HFProxyURL             *string        `json:"hf_proxy_url,omitempty"`
	HFToken                string         `json:"hf_token"`
	HFAllowedHosts         []string       `json:"hf_allowed_hosts"`
	HFTimeout              time.Duration  `json:"hf_timeout"`
	S3Endpoint             string         `json:"s3_endpoint"`
	S3Region               string         `json:"s3_region"`
	S3Bucket               string         `json:"s3_bucket"`
	S3Prefix               string         `json:"s3_prefix"`
	S3ForcePathStyle       bool           `json:"s3_force_path_style"`
	S3AccessKey            string         `json:"s3_access_key"`
	S3SecretKey            string         `json:"s3_secret_key"`
	S3ServerSideEncryption *bool          `json:"s3_server_side_encryption"`
	AuthMode               string         `json:"auth_mode"`
	AnonymousPublicRead    bool           `json:"anonymous_public_read"`
	SessionIdleTTL         time.Duration  `json:"session_idle_ttl"`
	SessionAbsoluteTTL     time.Duration  `json:"session_absolute_ttl"`
	CacheMaxFileSize       int64          `json:"cache_max_file_size"`
	CacheMaxTotalSize      *int64         `json:"cache_max_total_size,omitempty"`
	CacheEviction          *CacheEviction `json:"cache_eviction,omitempty"`
	DownloadMaxConcurrent  *int64         `json:"download_max_concurrent,omitempty"`
	DownloadRateLimit      *int64         `json:"download_rate_limit,omitempty"`
	DownloadDailyQuota     *int64         `json:"download_daily_quota,omitempty"`
	CustomRepos            *bool          `json:"custom_repositories,omitempty"`
	SafeFormatsOnly        *bool          `json:"safe_formats_only,omitempty"`
	UploadMaxFileSize      *int64         `json:"upload_max_file_size,omitempty"`
	UploadUserQuota        *int64         `json:"upload_user_quota,omitempty"`
	CacheRefTTL            time.Duration  `json:"cache_ref_ttl"`
	CacheNegativeTTL       time.Duration  `json:"cache_negative_ttl"`
	CacheGCGrace           time.Duration  `json:"cache_gc_grace"`
	OIDCIssuer             string         `json:"oidc_issuer"`
	OIDCClientID           string         `json:"oidc_client_id"`
	OIDCClientSecret       string         `json:"oidc_client_secret"`
	OIDCRedirectURL        string         `json:"oidc_redirect_url"`
	OIDCAutoProvision      bool           `json:"oidc_auto_provision"`
	LDAPURL                string         `json:"ldap_url"`
	LDAPStartTLS           bool           `json:"ldap_starttls"`
	LDAPBindDN             string         `json:"ldap_bind_dn"`
	LDAPBindPassword       string         `json:"ldap_bind_password"`
	LDAPBaseDN             string         `json:"ldap_base_dn"`
	LDAPUserFilter         string         `json:"ldap_user_filter"`
	LDAPAutoProvision      bool           `json:"ldap_auto_provision"`
	MetricsEnabled         bool           `json:"metrics_enabled"`
	MetricsPath            string         `json:"metrics_path"`
	TracingEnabled         bool           `json:"tracing_enabled"`
	TracingServiceName     string         `json:"tracing_service_name"`
	TracingSamplingRatio   float64        `json:"tracing_sampling_ratio"`
	OTLPEndpoint           string         `json:"otlp_endpoint"`
	CacheInventoryRefresh  time.Duration  `json:"cache_inventory_refresh"`
	LogLevel               string         `json:"log_level"`
}

// ErrSettingsConflict means the saved settings changed since the caller read
// them: saving would overwrite someone else's change.
var ErrSettingsConflict = errors.New("the saved settings changed since they were read")

// settingsRevision identifies one saved version of the settings; "" is none
// saved. It is a digest of the sealed value, so it carries no secret.
func settingsRevision(ciphertext []byte) string {
	if len(ciphertext) == 0 {
		return ""
	}
	sum := sha256.Sum256(ciphertext)
	return hex.EncodeToString(sum[:16])
}

// PersistedRevision returns the revision of the settings saved now.
func PersistedRevision(ctx context.Context, sqlDB *sql.DB) (string, error) {
	var ciphertext []byte
	err := sqlDB.QueryRowContext(ctx, `SELECT ciphertext FROM app_settings WHERE setting_key='runtime'`).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read saved settings revision: %w", err)
	}
	return settingsRevision(ciphertext), nil
}

func LoadPersisted(ctx context.Context, sqlDB *sql.DB, base Config) (Config, error) {
	var ciphertext []byte
	err := sqlDB.QueryRowContext(ctx, `SELECT ciphertext FROM app_settings WHERE setting_key='runtime'`).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		base.SettingsRevision = ""
		return base, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("load saved settings: %w", err)
	}
	plaintext, _, err := secretRing(base).Open(ciphertext)
	if err != nil {
		return Config{}, fmt.Errorf("saved settings cannot be decrypted; check HFPROXY_SESSION_SECRET and HFPROXY_SESSION_SECRET_PREVIOUS")
	}
	var saved managedSettings
	if err := json.Unmarshal(plaintext, &saved); err != nil {
		return Config{}, fmt.Errorf("saved settings are invalid")
	}
	saved.pinToEnvironment(base)
	loaded := saved.apply(base)
	loaded.SettingsRevision = settingsRevision(ciphertext)
	return loaded, nil
}

// SavePersisted saves the settings and returns their new revision. cfg must
// carry the revision it was derived from (SettingsRevision); when the saved
// settings are no longer that version the save is refused with
// ErrSettingsConflict instead of overwriting the other change.
func SavePersisted(ctx context.Context, sqlDB *sql.DB, cfg Config, actorID string) (string, error) {
	plaintext, err := json.Marshal(managedFromConfig(cfg))
	if err != nil {
		return "", fmt.Errorf("encode settings")
	}
	ciphertext, err := secretRing(cfg).Seal(plaintext)
	if err != nil {
		return "", fmt.Errorf("encrypt settings")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("save settings: %w", err)
	}
	defer tx.Rollback()
	// Nothing saved yet has no row to lock; two first saves are ordered by the
	// insert below, whose conflict is reported as a conflict too.
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT ciphertext FROM app_settings WHERE setting_key='runtime' FOR UPDATE`).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("save settings: %w", err)
	}
	if settingsRevision(stored) != cfg.SettingsRevision {
		return "", ErrSettingsConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		result, err := tx.ExecContext(ctx, `INSERT INTO app_settings(setting_key,ciphertext,updated_by,updated_at)
			VALUES('runtime',$1,NULLIF($2,''),now()) ON CONFLICT(setting_key) DO NOTHING`, ciphertext, actorID)
		if err != nil {
			return "", fmt.Errorf("save settings: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return "", ErrSettingsConflict
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE app_settings SET ciphertext=$1,updated_by=NULLIF($2,''),updated_at=now() WHERE setting_key='runtime'`, ciphertext, actorID); err != nil {
		return "", fmt.Errorf("save settings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("save settings: %w", err)
	}
	return settingsRevision(ciphertext), nil
}

func secretRing(cfg Config) *keyring.Ring {
	return keyring.NewRing(cfg.SessionSecret, cfg.PreviousSessionSecrets...)
}

// ReencryptPersisted seals the saved settings again under the current session
// secret. It reports whether anything had to change; settings already sealed
// under the current secret are left alone, and none saved is not an error.
func ReencryptPersisted(ctx context.Context, sqlDB *sql.DB, cfg Config) (bool, error) {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("start settings re-encryption: %w", err)
	}
	defer tx.Rollback()
	var ciphertext []byte
	err = tx.QueryRowContext(ctx, `SELECT ciphertext FROM app_settings WHERE setting_key='runtime' FOR UPDATE`).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load saved settings: %w", err)
	}
	ring := secretRing(cfg)
	plaintext, current, err := ring.Open(ciphertext)
	if err != nil {
		return false, fmt.Errorf("saved settings cannot be decrypted; check HFPROXY_SESSION_SECRET and HFPROXY_SESSION_SECRET_PREVIOUS")
	}
	if current {
		return false, nil
	}
	sealed, err := ring.Seal(plaintext)
	if err != nil {
		return false, fmt.Errorf("encrypt settings")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE app_settings SET ciphertext=$1,updated_at=now() WHERE setting_key='runtime'`, sealed); err != nil {
		return false, fmt.Errorf("save settings: %w", err)
	}
	return true, tx.Commit()
}

func DeletePersisted(ctx context.Context, sqlDB *sql.DB) error {
	_, err := sqlDB.ExecContext(ctx, `DELETE FROM app_settings WHERE setting_key='runtime'`)
	if err != nil {
		return fmt.Errorf("reset saved settings: %w", err)
	}
	return nil
}

func managedFromConfig(c Config) managedSettings {
	return managedSettings{
		PublicURL: c.PublicURL, HFBaseURL: c.HFBaseURL, HFProxyURL: stringPointer(c.HFProxyURL), HFToken: c.HFToken, HFAllowedHosts: c.HFAllowedHosts, HFTimeout: c.HFTimeout,
		S3Endpoint: c.S3Endpoint, S3Region: c.S3Region, S3Bucket: c.S3Bucket, S3Prefix: c.S3Prefix, S3ForcePathStyle: c.S3ForcePathStyle,
		S3AccessKey: c.S3AccessKey, S3SecretKey: c.S3SecretKey, S3ServerSideEncryption: boolPointer(c.S3ServerSideEncryption), AuthMode: c.AuthMode, AnonymousPublicRead: c.AnonymousPublicRead,
		SessionIdleTTL: c.SessionIdleTTL, SessionAbsoluteTTL: c.SessionAbsoluteTTL, CacheMaxFileSize: c.CacheMaxFileSize, CacheMaxTotalSize: int64Pointer(c.CacheMaxTotalSize), CacheEviction: &c.CacheEviction, DownloadMaxConcurrent: int64Pointer(c.DownloadMaxConcurrent), DownloadRateLimit: int64Pointer(c.DownloadRateLimit), DownloadDailyQuota: int64Pointer(c.DownloadDailyQuota), CustomRepos: boolPointer(c.CustomRepos), SafeFormatsOnly: boolPointer(c.SafeFormatsOnly), UploadMaxFileSize: int64Pointer(c.UploadMaxFileSize), UploadUserQuota: int64Pointer(c.UploadUserQuota),
		CacheRefTTL: c.CacheRefTTL, CacheNegativeTTL: c.CacheNegativeTTL, CacheGCGrace: c.CacheGCGrace,
		OIDCIssuer: c.OIDCIssuer, OIDCClientID: c.OIDCClientID, OIDCClientSecret: c.OIDCClientSecret, OIDCRedirectURL: c.OIDCRedirectURL,
		OIDCAutoProvision: c.OIDCAutoProvision, LDAPURL: c.LDAPURL, LDAPStartTLS: c.LDAPStartTLS, LDAPBindDN: c.LDAPBindDN,
		LDAPBindPassword: c.LDAPBindPassword, LDAPBaseDN: c.LDAPBaseDN, LDAPUserFilter: c.LDAPUserFilter, LDAPAutoProvision: c.LDAPAutoProvision,
		MetricsEnabled: c.MetricsEnabled, MetricsPath: c.MetricsPath, TracingEnabled: c.TracingEnabled, TracingServiceName: c.TracingServiceName, TracingSamplingRatio: c.TracingSamplingRatio, OTLPEndpoint: c.OTLPEndpoint, CacheInventoryRefresh: c.CacheInventoryRefresh, LogLevel: c.LogLevel,
	}
}

func (s managedSettings) apply(c Config) Config {
	c.PublicURL, c.HFBaseURL, c.HFToken, c.HFAllowedHosts, c.HFTimeout = s.PublicURL, s.HFBaseURL, s.HFToken, s.HFAllowedHosts, s.HFTimeout
	if s.HFProxyURL != nil {
		c.HFProxyURL = *s.HFProxyURL
	}
	c.S3Endpoint, c.S3Region, c.S3Bucket, c.S3Prefix, c.S3ForcePathStyle = s.S3Endpoint, s.S3Region, s.S3Bucket, s.S3Prefix, s.S3ForcePathStyle
	c.S3AccessKey, c.S3SecretKey, c.AuthMode, c.AnonymousPublicRead = s.S3AccessKey, s.S3SecretKey, s.AuthMode, s.AnonymousPublicRead
	if s.S3ServerSideEncryption != nil {
		c.S3ServerSideEncryption = *s.S3ServerSideEncryption
	}
	c.SessionIdleTTL, c.SessionAbsoluteTTL, c.CacheMaxFileSize = s.SessionIdleTTL, s.SessionAbsoluteTTL, s.CacheMaxFileSize
	// Settings saved before this limit existed leave it unset, so the value from
	// the environment is kept rather than reset to "unlimited".
	if s.CacheMaxTotalSize != nil {
		c.CacheMaxTotalSize = *s.CacheMaxTotalSize
	}
	if s.CacheEviction != nil {
		c.CacheEviction = *s.CacheEviction
	}
	// Settings saved before the download limits existed leave them unset, so
	// the values from the environment are kept.
	if s.DownloadMaxConcurrent != nil {
		c.DownloadMaxConcurrent = *s.DownloadMaxConcurrent
	}
	if s.DownloadRateLimit != nil {
		c.DownloadRateLimit = *s.DownloadRateLimit
	}
	if s.DownloadDailyQuota != nil {
		c.DownloadDailyQuota = *s.DownloadDailyQuota
	}
	// Likewise for publishing: settings saved before it existed keep the environment's values.
	if s.CustomRepos != nil {
		c.CustomRepos = *s.CustomRepos
	}
	if s.SafeFormatsOnly != nil {
		c.SafeFormatsOnly = *s.SafeFormatsOnly
	}
	if s.UploadMaxFileSize != nil {
		c.UploadMaxFileSize = *s.UploadMaxFileSize
	}
	if s.UploadUserQuota != nil {
		c.UploadUserQuota = *s.UploadUserQuota
	}
	c.CacheRefTTL, c.CacheNegativeTTL, c.CacheGCGrace = s.CacheRefTTL, s.CacheNegativeTTL, s.CacheGCGrace
	c.OIDCIssuer, c.OIDCClientID, c.OIDCClientSecret, c.OIDCRedirectURL = s.OIDCIssuer, s.OIDCClientID, s.OIDCClientSecret, s.OIDCRedirectURL
	c.OIDCAutoProvision, c.LDAPURL, c.LDAPStartTLS = s.OIDCAutoProvision, s.LDAPURL, s.LDAPStartTLS
	c.LDAPBindDN, c.LDAPBindPassword, c.LDAPBaseDN, c.LDAPUserFilter, c.LDAPAutoProvision = s.LDAPBindDN, s.LDAPBindPassword, s.LDAPBaseDN, s.LDAPUserFilter, s.LDAPAutoProvision
	c.MetricsEnabled, c.MetricsPath = s.MetricsEnabled, s.MetricsPath
	c.TracingEnabled, c.TracingServiceName, c.TracingSamplingRatio, c.OTLPEndpoint = s.TracingEnabled, s.TracingServiceName, s.TracingSamplingRatio, s.OTLPEndpoint
	if s.CacheInventoryRefresh > 0 {
		c.CacheInventoryRefresh = s.CacheInventoryRefresh
	}
	// Settings saved before the level was editable leave it empty, so the
	// value from the environment is kept.
	if level, err := logging.Normalize(s.LogLevel); err == nil && s.LogLevel != "" {
		c.LogLevel = level
	}
	return c
}

// withDefaultedEviction fills in the defaults of the eviction settings. The
// console reads them in that form and sends them back, so a comparison between
// the environment's raw value and a saved one would report a change nobody
// made, and would refuse every save while the setting is locked.
func withDefaultedEviction(c Config) Config {
	c.CacheEviction = c.CacheEviction.Normalized()
	return c
}

// OverriddenFields lists the settings whose effective value differs from the
// one read from the environment because saved settings took precedence. Names
// only: values may be secrets.
func OverriddenFields(fromEnvironment, effective Config) []string {
	fromEnvironment, effective = withDefaultedEviction(fromEnvironment), withDefaultedEviction(effective)
	before, after := reflect.ValueOf(fromEnvironment), reflect.ValueOf(effective)
	var names []string
	for i := 0; i < before.NumField(); i++ {
		if !reflect.DeepEqual(before.Field(i).Interface(), after.Field(i).Interface()) {
			names = append(names, before.Type().Field(i).Name)
		}
	}
	return names
}

func boolPointer(value bool) *bool       { return &value }
func int64Pointer(value int64) *int64    { return &value }
func stringPointer(value string) *string { return &value }

// SettingSource describes where one setting's effective value comes from.
type SettingSource struct {
	Name   string
	Source string // "environment" or "console" (saved settings)
	Value  string // redacted: secrets are never shown
}

// DescribeSources reports, for every setting, whether the effective value
// comes from the environment or from settings saved in the console.
func DescribeSources(fromEnvironment, effective Config) []SettingSource {
	overridden := map[string]bool{}
	for _, name := range OverriddenFields(fromEnvironment, effective) {
		overridden[name] = true
	}
	v, t := reflect.ValueOf(effective), reflect.TypeOf(effective)
	out := make([]SettingSource, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		source := "environment"
		if overridden[name] {
			source = "console"
		}
		out = append(out, SettingSource{Name: name, Source: source, Value: redactedValue(name, v.Field(i).Interface())})
	}
	return out
}

func redactedValue(name string, value any) string {
	text := fmt.Sprint(value)
	switch value := value.(type) {
	case string:
		text = value
	case []string:
		text = strings.Join(value, ",")
	}
	for _, marker := range []string{"Secret", "Token", "Password", "AccessKey"} {
		if strings.Contains(name, marker) {
			if text == "" || text == "[]" {
				return "(unset)"
			}
			return "(set, hidden)"
		}
	}
	if name == "DatabaseURL" || name == "HFProxyURL" || name == "OTLPEndpoint" {
		if parsed, err := url.Parse(text); err == nil && parsed.User != nil {
			parsed.User = url.User("***")
			return parsed.String()
		}
	}
	if text == "" {
		return "(unset)"
	}
	return text
}

// LockAll is the value of HFPROXY_LOCKED_SETTINGS that locks every setting.
const LockAll = "all"

// ManagedSettingNames lists the JSON names of the settings editable from the
// console, in declaration order.
func ManagedSettingNames() []string {
	t := reflect.TypeOf(managedSettings{})
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		names = append(names, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
	}
	return names
}

// ValidateLockedSettings rejects names that are not console-editable settings,
// so a typo cannot silently leave a setting unlocked.
func ValidateLockedSettings(locked []string) error {
	known := map[string]bool{LockAll: true}
	for _, name := range ManagedSettingNames() {
		known[name] = true
	}
	for _, name := range locked {
		if !known[name] {
			return fmt.Errorf("HFPROXY_LOCKED_SETTINGS contains %q, which is not a console setting", name)
		}
	}
	return nil
}

// LockedSet returns the names to treat as locked, expanding "all".
func LockedSet(locked []string) map[string]bool {
	set := map[string]bool{}
	for _, name := range locked {
		if name == LockAll {
			for _, every := range ManagedSettingNames() {
				set[every] = true
			}
			continue
		}
		set[name] = true
	}
	return set
}

// pinToEnvironment replaces the saved value of every locked setting with the
// one from the environment, so editing the Helm values or the compose file
// always wins for those.
func (s *managedSettings) pinToEnvironment(base Config) {
	locked := LockedSet(base.LockedSettings)
	if len(locked) == 0 {
		return
	}
	fromEnvironment := reflect.ValueOf(managedFromConfig(base))
	saved := reflect.ValueOf(s).Elem()
	t := fromEnvironment.Type()
	for i := 0; i < t.NumField(); i++ {
		if locked[strings.Split(t.Field(i).Tag.Get("json"), ",")[0]] {
			saved.Field(i).Set(fromEnvironment.Field(i))
		}
	}
}

// ChangedLocked returns the locked settings whose value differs between two
// configurations.
func ChangedLocked(locked []string, before, after Config) []string {
	set := LockedSet(locked)
	a, b := reflect.ValueOf(managedFromConfig(withDefaultedEviction(before))), reflect.ValueOf(managedFromConfig(withDefaultedEviction(after)))
	t := a.Type()
	var changed []string
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if set[name] && !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
			changed = append(changed, name)
		}
	}
	return changed
}

// LockedNames returns the locked settings, sorted, with "all" expanded.
func LockedNames(locked []string) []string {
	set := LockedSet(locked)
	names := make([]string, 0, len(set))
	for _, name := range ManagedSettingNames() {
		if set[name] {
			names = append(names, name)
		}
	}
	return names
}
