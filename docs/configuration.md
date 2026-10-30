# Configuration and deployment reference

The full reference for running hfproxy: the console settings, the command line, every environment variable, cache and resilience behavior, OIDC and LDAP, and container deployment. The [README](../README.md) has the short version.

## Console and settings

The console is served by the Go service at `/`. On a new instance it first offers to create the first administrator (see [First start](operations.md#first-start)). Open **Settings** to configure the Hugging Face Hub URL, optional upstream token and HTTP, HTTPS, SOCKS4, or SOCKS5 proxy, Garage or another S3-compatible endpoint/bucket/credentials, cache limits, public URL, and session lifetimes. Changes are validated and applied immediately; the Hub and S3 settings have connection checks. Credentials are write-only in the API and encrypted in PostgreSQL with AES-GCM.

The console is available in English (the default), French, Spanish, German, Portuguese, Simplified Chinese and Japanese. Use the language selector on the sign-in page or in the header; the choice is remembered in the browser and the page reloads in that language. Dates and numbers follow the chosen language. The text of API errors returned by the service, and the server-rendered `/login` page, stay in English. See [Translating the console](../ui/README.md#translating-the-console) to fix a translation or add a language.

The Angular source is in `ui/`. For frontend development, start the Go service on port 8080, then run `nvm use && npm ci && npm start` from `ui/`; `.nvmrc` pins Node 24.22.1. Angular serves the UI on port 4200 and proxies API requests to Go. The Docker build compiles and embeds the production UI.

The database URL, listen address, and `SESSION_SECRET` remain bootstrap settings in the environment. `SESSION_SECRET` protects access tokens, sessions and the saved settings, each with its own key derived from it. Do not simply replace it: follow [Rotating the session secret](operations.md#rotating-the-session-secret), which keeps existing tokens and settings working. Changing the S3 bucket or prefix does not move existing objects.

For a fresh installation that should configure S3 only through the console, leave `HFPROXY_S3_BUCKET` unset in the environment. hfproxy will serve the sign-in and setup pages, while `/readyz` stays unavailable until a valid bucket is saved. PostgreSQL and `SESSION_SECRET` are still needed at startup. The provided Compose setup preconfigures a single-node Garage instance and its default bucket for local use.

Local username/password authentication is enabled by default. OIDC/SSO and LDAP are optional and remain disabled until configured in **Settings**. OIDC requires an issuer, client ID, client secret, and callback URL; LDAP requires an LDAPS or StartTLS URL and a search base/filter. The first administrator is created from the console on first start, or with the CLI; additional local users and their roles can be managed in **Users**.

## Commands

    hfproxy serve
    hfproxy migrate up|status
    hfproxy admin create-user --username NAME --password-file PATH
    hfproxy admin reset-password --username NAME --password-file PATH
    hfproxy admin token create|list|revoke
    hfproxy admin repo add
    hfproxy admin identity link
    hfproxy admin rotate-secret
    hfproxy cache inspect|purge|verify
    hfproxy cache pin|unpin|pins
    hfproxy config validate
    hfproxy config show --source
    hfproxy version

`cache purge` is a dry run by default; execution requires `--execute --yes`. `cache verify` reads the objects and checks their SHA-256 hashes. Secrets can be supplied through `HFPROXY_*_FILE`. `config validate` does not reveal configuration values.

**Settings precedence.** Once settings are saved from the console they take precedence over the matching `HFPROXY_*` variables (environment-only settings such as `STAGING_DIR` are never overridden). At startup the server logs the names of the settings that the console overrides. `hfproxy config show --source` prints every setting with its origin (`environment` or `console`); secrets are never printed. To keep a setting under GitOps control, list it in `HFPROXY_LOCKED_SETTINGS` (comma-separated console setting names such as `s3_bucket,cache_max_total_size`, or `all`): saved values are then ignored for it, the console shows the field read-only, and an attempt to change it through the API is refused with 409. Misspelled names stop the server from starting. To go back to the environment values for the others, reset the saved settings from the console.

## Configuration

Environment variables use the `HFPROXY_` prefix. Common settings for local Compose are defined in `.env.example`.

| Area | Main variables |
| --- | --- |
| HTTP and database | LISTEN_ADDR, PUBLIC_URL, DATABASE_URL, AUTO_MIGRATE |
| Logging | LOG_LEVEL (`debug`, `info`, `warn`, `error`; also editable in the console, see [Logging](operations.md#logging)) |
| First start | SETUP_TOKEN or SETUP_TOKEN_FILE (optional fixed setup code, 16–256 characters; see [First start](operations.md#first-start)) |
| Download limits | DOWNLOAD_MAX_CONCURRENT_PER_USER, DOWNLOAD_RATE_LIMIT, DOWNLOAD_DAILY_QUOTA (all off by default; also in the console, see [Download limits](operations.md#download-limits)) |
| Hugging Face | HF_BASE_URL, HF_PROXY_URL, HF_TOKEN, HF_ALLOWED_REDIRECT_HOSTS, HF_ALLOWED_PRIVATE_CIDRS (a mirror on a private network; see [Network access](production.md#upstream-addresses)), HF_TIMEOUT |
| S3 | S3_ENDPOINT, S3_REGION, S3_BUCKET, S3_PREFIX, S3_FORCE_PATH_STYLE, S3_SERVER_SIDE_ENCRYPTION, S3_ACCESS_KEY, S3_SECRET_KEY |
| Authentication | AUTH_MODE=local or oidc, SESSION_SECRET, SESSION_SECRET_PREVIOUS, SESSION_IDLE_TTL, SESSION_ABSOLUTE_TTL, ANONYMOUS_PUBLIC_READ |
| OIDC | OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL, OIDC_AUTO_PROVISION |
| LDAP | LDAP_URL, LDAP_STARTTLS, LDAP_BIND_DN, LDAP_BIND_PASSWORD, LDAP_BASE_DN, LDAP_USER_FILTER, LDAP_AUTO_PROVISION |
| Cache limits | CACHE_MAX_FILE_SIZE, CACHE_REF_TTL, CACHE_NEGATIVE_TTL, CACHE_GC_GRACE |
| Cache strategies | CACHE_EVICTION_STRATEGY, CACHE_ADMISSION and the limits of [Cache strategies](cache-strategies.md) (also in the console) |
| Capacity and retention | CACHE_MAX_TOTAL_SIZE, CACHE_FILL_CONCURRENCY, STAGING_DIR, STAGING_MIN_FREE_BYTES, RETENTION_DOWNLOADS, RETENTION_AUDIT |
| Metrics | METRICS_ENABLED, METRICS_PATH, METRICS_ADDR |
| Safe formats | SAFE_FORMATS_ONLY (also in the console; see [Safe formats](safe-formats.md)) |
| Publishing | CUSTOM_REPOSITORIES, UPLOAD_MAX_FILE_SIZE, UPLOAD_USER_QUOTA (also in the console, see [Publishing](publishing.md#limits)) |
| Hub resilience | UPSTREAM_BREAKER_THRESHOLD, UPSTREAM_BREAKER_COOLDOWN, API_CACHE_MAX_BYTES, API_CACHE_MAX_STALE |

PostgreSQL is required. The S3 bucket must exist, remain private, and allow the service identity to read, write, and delete objects. Standard AWS credentials (IAM role, AWS environment variables, or a mounted profile) work when the S3 access key variables are unset. Use `S3_FORCE_PATH_STYLE=true` with the local Garage endpoint. Garage does not implement S3-managed AES256 encryption, so the Compose setup disables the `S3_SERVER_SIDE_ENCRYPTION` request header; use encrypted disks for at-rest encryption. Other S3 services can keep that option enabled if they support it.

The upstream HF token is optional for public repositories and required for private or gated repositories. It is sent only to the configured HF host, never to the client. Redirects are followed over HTTPS to allowed HF hosts. Add the delivery hosts observed in your environment to `HF_ALLOWED_REDIRECT_HOSTS`.

Capacity and retention settings come from the environment, except the cache size limit, which can also be changed in the console:

- `CACHE_MAX_TOTAL_SIZE` (bytes, `0` = unlimited): when live blobs exceed it, artifacts are dropped until usage is under 90% of the quota, least recently used first unless another [strategy](cache-strategies.md) is chosen. Their objects are deleted by the garbage collector after `CACHE_GC_GRACE`. It can also be set under **Settings → Cache rules**; a value saved there replaces the environment's and is enforced within seconds, without a restart. Leaving it out of an API request keeps the current limit, and `0` removes it.
- `CACHE_FILL_CONCURRENCY` (default `4`): how many files may be fetched from the Hub at once. Further requests wait up to 15 seconds, then receive `503` with `Retry-After`.
- `STAGING_DIR` (default: the system temporary directory): where files are staged before upload. Size it for `CACHE_FILL_CONCURRENCY` times your largest file; a fill that does not fit in the free space is refused with `507`. `hfproxy_staging_free_bytes` and `hfproxy_staging_reserved_bytes` show the room left.
- `STAGING_MIN_FREE_BYTES` (default `0`, off): when set, `/readyz` answers `503` with `"reason":"staging_space"` while the staging directory has less free space than this. Use it when several instances share traffic, so a full disk takes that instance out of rotation; with a single instance it would also stop cache hits, so leave it off and alert on the gauge instead.
- `AUTH_CACHE_TTL` (default `5s`, `0` turns it off) and `DOWNLOAD_LOG_QUEUE` (default `1024`, `0` writes synchronously): keep database writes and lookups off the request path. A token or session lookup is reused for `AUTH_CACHE_TTL` (a measured 359 µs lookup against a local PostgreSQL becomes 3 µs); revoking a token, ending a session or changing a user clears it at once on every replica: the instance that makes the change (or the CLI, for `admin token revoke` and `admin reset-password`) sends a PostgreSQL notification that each replica listens for on a connection of its own, and the TTL is only the bound if a notification is lost (the listener reconnects by itself and empties the cache when it does). Cache-hit download events are queued and written in batches; when the queue is full the request writes its own event, so none is lost, and the queue is flushed at shutdown.
- `RETENTION_DOWNLOADS` (default `2160h`) and `RETENTION_AUDIT` (default `8760h`): Go durations after which download events and audit events are deleted; `0` keeps them. Expired sessions are always purged.
- `METRICS_ADDR` (for example `127.0.0.1:9090`): serve metrics on a dedicated listener instead of the public port. That listener has no authentication and must stay on an internal network. Without it, `/metrics` is served on the public port and requires an operator or administrator: send a token with write scope as a bearer token (`authorization: {credentials: ...}` in the Prometheus scrape config).

When `CACHE_MAX_TOTAL_SIZE` is set, files you cannot afford to lose can be **pinned**. A pin covers every revision of a repository, one commit, or a branch or tag, and eviction never touches what a pin covers:

    hfproxy cache pin --repo org/model                    # every revision
    hfproxy cache pin --repo org/model --commit <full SHA>
    hfproxy cache pin --repo org/model --ref main         # a branch or tag
    hfproxy cache unpin --repo org/model [--commit <full SHA> | --ref main]
    hfproxy cache pins                                    # what is pinned and how much it holds

A pinned branch or tag **follows** the name: it protects the commit the name points to now, and when the branch moves to a new commit the old one stops being protected. hfproxy learns where a name points when a client requests it, so a branch nobody has requested yet protects nothing, and `cache pins` says so. Pin a commit SHA to hold one exact version.

The same is available to administrators at `GET|POST|DELETE /api/v1/repos/{id}/pins`: the body of `POST` carries `{"commit": "<SHA>"}` or `{"ref": "main"}` (or `{}` for the whole repository), and `DELETE` takes `?commit=` or `?ref=`. Pinned files still count towards the quota, so pinning more than the quota allows leaves the cache above its limit; a warning is logged and `hfproxy_cache_pinned_bytes` shows how much is pinned. `cache purge` is an explicit action and does not honor pins.

If the Hub is unreachable or answers 429/5xx and a branch or tag was resolved before, hfproxy serves the cached file for the last known commit with `X-Cache: STALE`. Transient Hub errors are retried twice with backoff.

Two more protections keep an unhealthy Hub from dragging the proxy down. Both read the environment only:

- **Circuit breaker.** After `UPSTREAM_BREAKER_THRESHOLD` consecutive failures (default `5`; network errors, timeouts, 429, 502, 503 and 504, never 404 or other client errors) the circuit for that Hub host opens for `UPSTREAM_BREAKER_COOLDOWN` (default `30s`). Requests then fail at once, or are answered from the cache, instead of each waiting through timeouts and retries. After the cooldown one request probes the host and the circuit closes if it succeeds. `0` disables it.
- **Metadata cache.** Responses to `/api/{models,datasets,spaces}/...` (repository info, revisions, trees, commits) are kept in memory, up to `API_CACHE_MAX_BYTES` (default 32 MiB, `0` disables). An entry is served as is for `CACHE_REF_TTL`, then revalidated with its ETag; if the Hub is failing, an entry up to `API_CACHE_MAX_STALE` old (default `24h`) is served with `X-Cache: STALE`. Simultaneous identical requests share one Hub request. Access to the repository is always checked first, and `HEAD` requests are not cached.

See [docs/production.md](production.md) before exposing hfproxy to other users.

## OIDC and LDAP

OIDC uses Authorization Code, PKCE S256, state, nonce, and ID token validation. By default, an OIDC identity must be linked to a local account by an administrator using the CLI. Optional auto-provisioning creates reader accounts only and requires a verified email address.

    hfproxy admin identity link --provider oidc --username admin --issuer https://idp.example/issuer --subject SUBJECT_FROM_ID_TOKEN

LDAP supports LDAPS or LDAP with StartTLS. The user filter must contain `{username}`; the value is escaped before the search. The proxy searches for a single entry, then binds with the supplied password. It does not retain that password. LDAP auto-provisioning is disabled by default; when enabled, new accounts receive the reader role.

LDAP identity and directory state:

- The link between a directory entry and its local account uses a stable identifier (`entryUUID`, or `objectGUID` on Active Directory) rather than the DN, so moving or renaming a user in the directory does not break it. Accounts linked by DN before this change are moved to the stable identifier at their next sign-in; directories that expose neither attribute keep using the DN.
- An account the directory marks as disabled or locked (`userAccountControl` flag 2, `nsAccountLock: true`, `pwdAccountLockedTime`) cannot sign in. Sessions and tokens issued earlier stay valid until they expire or an administrator disables the local account, which revokes them.
- `HFPROXY_LDAP_GROUP_SYNC=true` mirrors the user's directory groups into hfproxy groups with source `ldap` at every sign-in (`HFPROXY_LDAP_GROUP_BASE_DN`, default the user base; `HFPROXY_LDAP_GROUP_FILTER`, default `(member={dn})`, where `{dn}` is the escaped user DN; the group name is its `cn`). Memberships in local groups are never changed, and a group read failure leaves memberships as they were.

OIDC and LDAP provide sign-in to the same local role model (`admin`, `operator`, `reader`). Groups can be managed locally and granted access through allowlist policies.

## Containers and operations

The multi-stage Dockerfile builds a static binary and runs the final image as UID 65532. `docker-compose.test.yml` adds a Keycloak, a Prometheus and a Grafana to try sign-in and monitoring locally (see [docs/testing-stack.md](testing-stack.md)). `docker-compose.yml` is a local development environment with PostgreSQL and single-node Garage; it binds the proxy port to loopback only. Garage's single-node development setup has no redundancy and is not a production storage deployment. The development compose file has no built-in credentials: it needs the `.env` created from `.env.example`. `docker-compose.example.yml` (with `Caddyfile.example`) demonstrates secret injection through `*_FILE`, PostgreSQL with `sslmode=verify-full`, `HFPROXY_TRUSTED_PROXIES` and TLS termination by a reverse proxy. The server refuses to start with the published development secrets unless it is bound to loopback.

For production:

1. Set `PUBLIC_URL` and the OIDC callback to HTTPS URLs.
2. Disable `ANONYMOUS_PUBLIC_READ`, protect secrets with a dedicated secrets manager, and keep the bucket private.
3. Apply migrations with `hfproxy migrate up` during deployment; `AUTO_MIGRATE` is disabled by default.
4. Provide enough local temporary disk space for the configured maximum file size: cache misses are staged on disk before being published to S3.
5. Back up PostgreSQL and the bucket together. Restore both to preserve the links between artifacts and blobs.
6. Restrict egress to Hugging Face and validated HF CDN hosts. The upstream token should have only the minimum scopes and gated access required.

## Published Docker image

The release workflow builds and publishes a multi-platform image (`linux/amd64` and `linux/arm64`) to GitHub Container Registry whenever a semantic version tag is pushed. The image includes the administration UI, runs as a non-root user, and embeds its version and source commit.

Release tags, Docker image tags, and the embedded binary version use `MAJOR.MINOR.PATCH` without a leading `v`. For example, after publishing `0.0.4`:

    docker pull ghcr.io/hfproxy/hfproxy:0.0.4
    docker run --rm ghcr.io/hfproxy/hfproxy:0.0.4 version

Use a pinned version in production. The `latest` tag follows the newest stable version. To use the image in a Compose deployment, set the service image to `ghcr.io/hfproxy/hfproxy:0.0.4` instead of building it locally.

The first GHCR package is private by default. Sign in with `docker login ghcr.io` to pull a private image, or change the package visibility to Public in the package settings to allow anonymous pulls.

When running behind a reverse proxy, set `HFPROXY_TRUSTED_PROXIES` to a comma-separated list of the proxy CIDRs. hfproxy uses `X-Forwarded-For` for login rate limiting only when the direct peer is in this list; forwarded headers from other peers are ignored.

To publish a release, push a version tag without a leading `v`, for example `0.0.4`:

    git tag 0.0.4
    git push origin 0.0.4

Known-size Hub downloads are checkpointed in 64 MiB blocks in the S3-compatible bucket. Retrying a failed preload resumes from the last complete block when the upstream honors HTTP Range requests; otherwise hfproxy safely falls back to a full transfer. Abandoned checkpoints are removed after seven days. The current block is downloaded again if a transfer fails before that block is stored.

Health endpoints: `/healthz` provides liveness without dependency checks; `/readyz` checks PostgreSQL and S3. JSON logs include request ID, route, duration, status, and bytes, without query strings, bearer tokens, or signed URLs.
