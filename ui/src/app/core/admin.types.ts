export type Page =
  'overview' | 'repositories' | 'cache' | 'users' | 'groups' | 'audit' | 'settings';
export type RepoType = 'models' | 'datasets' | 'spaces';
export type Policy = 'public' | 'allowlist' | 'admin_only';

export interface Principal {
  id: string;
  username: string;
  email?: string;
  display_name?: string;
  role: string;
  authenticated?: boolean;
}

export interface FirstAdminInput {
  setup_code: string;
  username: string;
  display_name: string;
  email: string;
  password: string;
}

export interface AuthProviders {
  local: boolean;
  oidc: boolean;
  ldap: boolean;
}
export interface Stats {
  repositories: number;
  artifacts: number;
  cache_bytes: number;
  cache_size: string;
  cache_hits: number;
  cache_misses: number;
  hit_ratio: number;
  downloads_in_progress: number;
}

export interface ActiveDownload {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  path: string;
  bytes: number;
  started_at: string;
}

export interface PreloadJob {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  status:
    | 'queued'
    | 'running'
    | 'completed'
    | 'completed_with_errors'
    | 'failed'
    | 'interrupted'
    | 'incomplete';
  files_total: number;
  files_processed: number;
  failed_files: number;
  /** Files left out because only safe formats are served. */
  skipped_files?: number;
  error?: string;
  started_at: string;
  updated_at: string;
}

export interface Repository {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  access_policy: Policy;
  /** "hub" for a repository cached from the Hub, "local" for one published here. */
  origin?: 'hub' | 'local';
  /** Exempt from the safe-formats setting. */
  allow_unsafe_formats?: boolean;
  created_at: string;
  updated_at: string;
}

export interface PublishedRepo {
  id: string;
  repo_type: 'models' | 'datasets';
  repo_id: string;
  namespace: string;
  name: string;
  private: boolean;
  owner: string;
  bytes: number;
  created_at: string;
  updated_at: string;
  can_write: boolean;
}

export interface PublishedList {
  items: PublishedRepo[];
  enabled: boolean;
  namespaces: string[];
  max_file: number;
  quota: number;
}

export interface PublishedReadme {
  content?: string;
  card?: Record<string, unknown>;
  card_error?: string;
  size: number;
  truncated?: boolean;
}

export interface PublishedDetail {
  repository: PublishedRepo;
  revision: string;
  branches: string[];
  tags: string[];
  head: { sha: string; summary: string; author: string; date: string };
  files_url: string;
  readme?: PublishedReadme;
}

export interface PublishedEntry {
  name: string;
  path: string;
  type: 'file' | 'directory';
  size: number;
  lfs: boolean;
}

export interface PublishedCommit {
  sha: string;
  summary: string;
  description: string;
  author: string;
  date: string;
}

export interface CacheEntry {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  commit_sha: string;
  path: string;
  size_bytes: number;
  sha256: string;
  cached_at: string;
  last_accessed_at?: string;
}

export interface AdminUser {
  id: string;
  username: string;
  email: string;
  display_name: string;
  role: string;
  status: string;
  created_at: string;
  /** The user's own daily download quota in bytes: null uses the default, 0 is unlimited. */
  daily_quota_bytes?: number | null;
}

export interface AdminGroup {
  id: string;
  name: string;
  source: string;
  members: number;
}
export interface GroupMember {
  id: string;
  username: string;
  role: string;
}
export interface Grant {
  subject_type: string;
  subject_id: string;
  permission: string;
}
export interface AuditItem {
  id: string;
  actor: string;
  action: string;
  target_type: string;
  target_id: string;
  created_at: string;
}

export interface ManagedSettings {
  /** Version of the saved settings these come from; sent back when saving. */
  revision: string;
  public_url: string;
  hf_base_url: string;
  hf_proxy_set: boolean;
  hf_allowed_hosts: string[];
  hf_timeout: string;
  hf_token_set: boolean;
  s3_endpoint: string;
  s3_region: string;
  s3_bucket: string;
  s3_prefix: string;
  s3_force_path_style: boolean;
  s3_server_side_encryption: boolean;
  s3_access_key_set: boolean;
  s3_secret_key_set: boolean;
  auth_mode: 'local' | 'oidc';
  anonymous_public_read: boolean;
  session_idle_ttl: string;
  session_absolute_ttl: string;
  cache_max_file_size: number;
  cache_max_total_size: number;
  cache_eviction: CacheEviction;
  /** Download limits; 0 turns each one off. */
  download_max_concurrent: number;
  download_rate_limit: number;
  download_daily_quota: number;
  custom_repositories: boolean;
  safe_formats_only: boolean;
  upload_max_file_size: number;
  upload_user_quota: number;
  cache_ref_ttl: string;
  cache_negative_ttl: string;
  cache_gc_grace: string;
  oidc_issuer: string;
  oidc_client_id: string;
  oidc_client_secret_set: boolean;
  oidc_redirect_url: string;
  oidc_auto_provision: boolean;
  ldap_url: string;
  ldap_starttls: boolean;
  ldap_bind_dn: string;
  ldap_bind_password_set: boolean;
  ldap_base_dn: string;
  ldap_user_filter: string;
  ldap_auto_provision: boolean;
  metrics_enabled: boolean;
  metrics_path: string;
  tracing_enabled: boolean;
  tracing_service_name: string;
  tracing_sampling_ratio: number;
  otlp_endpoint: string;
  cache_inventory_refresh: string;
  /** Log verbosity: debug, info, warn or error. */
  log_level: 'debug' | 'info' | 'warn' | 'error';
  /** Settings pinned to the environment (HFPROXY_LOCKED_SETTINGS): read-only here. */
  locked: string[];
}

/** The cache eviction strategies the server knows, in the order the console shows them. */
export type CacheStrategy = 'lru' | 'cost_lru' | 'gdsf' | 'lfu' | 'fifo';

/** Whether a file is stored the first time it is asked for. */
export type CacheAdmissionPolicy = 'always' | 'second_request' | 'frequency';

/** Which files leave the cache, and when. Every limit is off at 0. */
export interface CacheEviction {
  strategy: CacheStrategy;
  target_percent: number;
  max_repository_size: number;
  max_namespace_size: number;
  max_revisions: number;
  max_idle_hours: number;
  fifo: { ring_size: number; ring_files: number };
  frequency: { half_life_hours: number };
  cost: { weight: number; bandwidth: number; latency_seconds: number };
  admission: { policy: CacheAdmissionPolicy; window_hours: number; min_size: number };
}

export interface SettingsDraft extends ManagedSettings {
  hf_allowed_hosts_text: string;
  hf_proxy_url: string;
  clear_hf_proxy: boolean;
  hf_token: string;
  clear_hf_token: boolean;
  s3_access_key: string;
  s3_secret_key: string;
  clear_s3_credentials: boolean;
  oidc_client_secret: string;
  clear_oidc_client_secret: boolean;
  ldap_bind_password: string;
  clear_ldap_bind_password: boolean;
}
