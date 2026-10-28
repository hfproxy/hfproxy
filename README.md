# hfproxy

<p align="center">
  <img src="ui/public/hfproxy-logo.svg" alt="hfproxy — Hugging Face Proxy" width="250">
</p>

<p align="center">
  <a href="https://github.com/hfproxy/hfproxy/actions/workflows/ci.yml"><img src="https://github.com/hfproxy/hfproxy/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI status: formatting, vet, tests, and build"></a>
  <a href="https://github.com/hfproxy/hfproxy/tags"><img src="https://img.shields.io/github/v/tag/hfproxy/hfproxy?label=version" alt="Latest version tag"></a>
  <a href="https://github.com/hfproxy/hfproxy/pkgs/container/hfproxy"><img src="https://img.shields.io/badge/container-GHCR-2496ED?logo=docker&amp;logoColor=white" alt="Container image on GitHub Container Registry"></a>
  <img src="https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue?logo=docker&amp;logoColor=white" alt="Supported container platforms: amd64 and arm64">
</p>

hfproxy is an internal Hugging Face Hub proxy written in Go. It applies local access policies before every request, fetches files from the Hub, deduplicates them by SHA-256 in a private S3-compatible bucket, and stores the catalog in PostgreSQL.

hfproxy includes a browser-based administration console built with Angular 22 and Tailwind CSS. It manages access policies and runtime service settings while keeping upstream credentials on the server.

The GitHub Actions CI workflow checks Go formatting, runs `go vet` and `go test ./...`, and builds the Go binary.

## Administration console

<table>
  <tr>
    <td width="50%" align="center"><img src="images/overview.png" alt="hfproxy administration overview showing repository, cache, storage, and hit-rate metrics" width="100%"></td>
    <td width="50%" align="center"><img src="images/repository.png" alt="hfproxy repository administration showing policies, cache status, and preload controls" width="100%"></td>
  </tr>
  <tr>
    <td align="center"><sub>Overview — cache and service health at a glance</sub></td>
    <td align="center"><sub>Repositories — access policies and preload status</sub></td>
  </tr>
</table>

## Run locally

The provided Compose setup is intended for development on a trusted machine. It uses demo passwords, HTTP on the internal Docker network, and allows anonymous reads only for repositories marked public.

    cp .env.example .env
    docker compose up --build -d

Create the first administrator. Put the password in a protected file or send it on stdin; it is never passed as a process argument.

    mkdir -p .secrets
    umask 077
    # Write the password to .secrets/admin-password using your secrets manager.
    docker compose exec -T hfproxy hfproxy admin create-user --username admin --password-file - < .secrets/admin-password

If the admin account already exists and you need to change its password, use `reset-password`. This keeps the database and other settings; existing web sessions for that admin are revoked.

    docker compose exec -T hfproxy hfproxy admin reset-password --username admin --password-file - < .secrets/admin-password

Register a repository and choose its policy before using it:

    docker compose exec hfproxy hfproxy admin repo add --type models --id bert-base-uncased --policy public

Open http://localhost:8080 and sign in. The overview shows the cache, policies, and recent artifacts. For HF clients, create an hfproxy token; its full value is shown only once:

    docker compose exec hfproxy hfproxy admin token create --username admin --name laptop

Tokens default to the `read` scope. For an administrator token that can call write endpoints, use `--scope write`; use `--scope admin` only when the owner is an administrator. Operators can create `read` or `write` tokens, while readers can create only `read` tokens.

Then configure a Python client:

    export HF_ENDPOINT=http://localhost:8080
    export HF_TOKEN=hfpx_your_hfproxy_token
    python -c 'from huggingface_hub import hf_hub_download; print(hf_hub_download("bert-base-uncased", "config.json"))'

When `HFPROXY_ANONYMOUS_PUBLIC_READ=true`, clients can read repositories registered with the public policy without a token. The administration endpoints and dashboard still require authentication.

### Administration console

The console is served by the Go service at `/` and is available after creating the first administrator. Open **Settings** to configure the Hugging Face Hub URL, optional upstream token and HTTP, HTTPS, SOCKS4, or SOCKS5 proxy, Garage or another S3-compatible endpoint/bucket/credentials, cache limits, public URL, and session lifetimes. Changes are validated and applied immediately; the Hub and S3 settings have connection checks. Credentials are write-only in the API and encrypted in PostgreSQL with AES-GCM.

The Angular source is in `ui/`. For frontend development, start the Go service on port 8080, then run `nvm use && npm ci && npm start` from `ui/`; `.nvmrc` pins Node 24.22.1. Angular serves the UI on port 4200 and proxies API requests to Go. The Docker build compiles and embeds the production UI.

The database URL, listen address, and `SESSION_SECRET` remain bootstrap settings in the environment. Keep `SESSION_SECRET` unchanged: it encrypts saved runtime settings as well as signing sessions. Changing it without migrating the saved settings will make those credentials unreadable. Changing the S3 bucket or prefix does not move existing objects.

For a fresh installation that should configure S3 only through the console, leave `HFPROXY_S3_BUCKET` unset in the environment. hfproxy will serve the sign-in and setup pages, while `/readyz` stays unavailable until a valid bucket is saved. PostgreSQL and `SESSION_SECRET` are still needed at startup. The provided Compose setup preconfigures a single-node Garage instance and its default bucket for local use.

Local username/password authentication is enabled by default. OIDC/SSO and LDAP are optional and remain disabled until configured in **Settings**. OIDC requires an issuer, client ID, client secret, and callback URL; LDAP requires an LDAPS or StartTLS URL and a search base/filter. The first administrator must still be created with the CLI; additional local users and their roles can be managed in **Users**.

## Commands

    hfproxy serve
    hfproxy migrate up|status
    hfproxy admin create-user --username NAME --password-file PATH
    hfproxy admin reset-password --username NAME --password-file PATH
    hfproxy admin token create|list|revoke
    hfproxy admin repo add
    hfproxy admin identity link
    hfproxy cache inspect|purge|verify
    hfproxy config validate
    hfproxy version

`cache purge` is a dry run by default; execution requires `--execute --yes`. `cache verify` reads the objects and checks their SHA-256 hashes. Secrets can be supplied through `HFPROXY_*_FILE`. `config validate` does not reveal configuration values.

## Configuration

Environment variables use the `HFPROXY_` prefix. Common settings for local Compose are defined in `.env.example`.

| Area | Main variables |
| --- | --- |
| HTTP and database | LISTEN_ADDR, PUBLIC_URL, DATABASE_URL, AUTO_MIGRATE, LOG_LEVEL |
| Hugging Face | HF_BASE_URL, HF_PROXY_URL, HF_TOKEN, HF_ALLOWED_REDIRECT_HOSTS, HF_TIMEOUT |
| S3 | S3_ENDPOINT, S3_REGION, S3_BUCKET, S3_PREFIX, S3_FORCE_PATH_STYLE, S3_SERVER_SIDE_ENCRYPTION, S3_ACCESS_KEY, S3_SECRET_KEY |
| Authentication | AUTH_MODE=local or oidc, SESSION_SECRET, SESSION_IDLE_TTL, SESSION_ABSOLUTE_TTL, ANONYMOUS_PUBLIC_READ |
| OIDC | OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL, OIDC_AUTO_PROVISION |
| LDAP | LDAP_URL, LDAP_STARTTLS, LDAP_BIND_DN, LDAP_BIND_PASSWORD, LDAP_BASE_DN, LDAP_USER_FILTER, LDAP_AUTO_PROVISION |
| Cache limits | CACHE_MAX_FILE_SIZE, CACHE_REF_TTL, CACHE_NEGATIVE_TTL, CACHE_GC_GRACE |

PostgreSQL is required. The S3 bucket must exist, remain private, and allow the service identity to read, write, and delete objects. Standard AWS credentials (IAM role, AWS environment variables, or a mounted profile) work when the S3 access key variables are unset. Use `S3_FORCE_PATH_STYLE=true` with the local Garage endpoint. Garage does not implement S3-managed AES256 encryption, so the Compose setup disables the `S3_SERVER_SIDE_ENCRYPTION` request header; use encrypted disks for at-rest encryption. Other S3 services can keep that option enabled if they support it.

The upstream HF token is optional for public repositories and required for private or gated repositories. It is sent only to the configured HF host, never to the client. Redirects are followed over HTTPS to allowed HF hosts. Add the delivery hosts observed in your environment to `HF_ALLOWED_REDIRECT_HOSTS`.

### OIDC and LDAP

OIDC uses Authorization Code, PKCE S256, state, nonce, and ID token validation. By default, an OIDC identity must be linked to a local account by an administrator using the CLI. Optional auto-provisioning creates reader accounts only and requires a verified email address.

    hfproxy admin identity link --provider oidc --username admin --issuer https://idp.example/issuer --subject SUBJECT_FROM_ID_TOKEN

LDAP supports LDAPS or LDAP with StartTLS. The user filter must contain `{username}`; the value is escaped before the search. The proxy searches for a single entry, then binds with the supplied password. It does not retain that password. LDAP auto-provisioning is disabled by default; when enabled, new accounts receive the reader role.

OIDC and LDAP provide sign-in to the same local role model (`admin`, `operator`, `reader`). Groups can be managed locally and granted access through allowlist policies.

## Hub API and compatibility

Supported Hub read routes:

- GET and HEAD `/{models|datasets|spaces}/{repo_id}/resolve/{revision}/{filepath}`
- GET `/api/{models|datasets|spaces}/{repo_id}`
- GET `/api/{type}/{repo_id}/revision/{revision}`
- GET `/api/{type}/{repo_id}/tree/{revision}[/{path}]`
- GET `/api/{type}/{repo_id}/commits/{revision}`
- GET `/api/whoami-v2` and `/api/health`

The proxy supports branch, tag, and SHA revisions; nested paths; HEAD; ETag/304; and simple Range requests from the cache. Mutable references are cached for `CACHE_REF_TTL` (60 seconds by default), while a SHA remains immutable. Multi-part ranges are explicitly rejected. Hub metadata uses an allowlist of routes; there is no generic URL proxy.

The operations API is under `/api/v1` and covers identity and providers, statistics, repositories and grants, cache, users (`GET`/`POST /users`, `PATCH /users/{id}`), groups (`GET`/`POST /groups`, `PATCH /groups/{id}`), audit, and settings (`GET`/`PUT /settings`). Errors use `application/problem+json`. Identity and settings operations require the admin role; cache purge and repository preload are available to operators and administrators. Browser mutations require CSRF validation.

A cache hit never bypasses authorization. Anonymous reads apply only to repositories explicitly registered as public. An allowlist policy grants access to explicitly authorized local users and groups; `admin_only` is the default policy.

## Containers and operations

The multi-stage Dockerfile builds a static binary and runs the final image as UID 65532. `docker-compose.yml` is a local development environment with PostgreSQL and single-node Garage; it binds the proxy port to loopback only. Garage's single-node development setup has no redundancy and is not a production storage deployment. `docker-compose.example.yml` demonstrates secret injection and TLS termination by a reverse proxy.

For production:

1. Set `PUBLIC_URL` and the OIDC callback to HTTPS URLs.
2. Disable `ANONYMOUS_PUBLIC_READ`, protect secrets with a dedicated secrets manager, and keep the bucket private.
3. Apply migrations with `hfproxy migrate up` during deployment; `AUTO_MIGRATE` is disabled by default.
4. Provide enough local temporary disk space for the configured maximum file size: cache misses are staged on disk before being published to S3.
5. Back up PostgreSQL and the bucket together. Restore both to preserve the links between artifacts and blobs.
6. Restrict egress to Hugging Face and validated HF CDN hosts. The upstream token should have only the minimum scopes and gated access required.

### Published Docker image

The release workflow builds and publishes a multi-platform image (`linux/amd64` and `linux/arm64`) to GitHub Container Registry whenever a semantic version tag is pushed. The image includes the administration UI, runs as a non-root user, and embeds its version and source commit.

The current release is `v0.0.3`:

    docker pull ghcr.io/hfproxy/hfproxy:v0.0.3
    docker run --rm ghcr.io/hfproxy/hfproxy:v0.0.3 version

Use a pinned version in production. The `latest` tag follows the newest stable version. To use the image in a Compose deployment, set the service image to `ghcr.io/hfproxy/hfproxy:v0.0.3` instead of building it locally.

The first GHCR package is private by default. Sign in with `docker login ghcr.io` to pull a private image, or change the package visibility to Public in the package settings to allow anonymous pulls.

To publish a later release, push another version tag, for example `v0.0.3`:

    git tag v0.0.3
    git push origin v0.0.3

Known-size Hub downloads are checkpointed in 64 MiB blocks in the S3-compatible bucket. Retrying a failed preload resumes from the last complete block when the upstream honors HTTP Range requests; otherwise hfproxy safely falls back to a full transfer. Abandoned checkpoints are removed after seven days. The current block is downloaded again if a transfer fails before that block is stored.

Health endpoints: `/healthz` provides liveness without dependency checks; `/readyz` checks PostgreSQL and S3. JSON logs include request ID, route, duration, status, and bytes, without query strings, bearer tokens, or signed URLs.

## Current limitations

- Native Xet/CAS, uploads, repository write operations, inference, and Spaces execution are not supported.
- The reference TTL applies to cache reads; a cache miss may still query the Hub API for metadata and to confirm the SHA before downloading.
- OIDC verifies identities and sessions, but IdP group claims are not automatically mapped to local groups.
- LDAP supports LDAPS/StartTLS and user binds; Active Directory group synchronization is not included.
- The administration console manages runtime settings, repositories and grants, users, groups, cache entries, and audit events. Database connection, listen address, and session encryption secret remain environment-managed bootstrap settings.
- Multi-part Range requests, Prometheus metrics, distributed rate limits, and paginated cache purge still need work before large-scale operation.
- Negative-result caching is in-memory per process; `CACHE_NEGATIVE_TTL` controls its lifetime. Orphaned blobs are collected periodically after `CACHE_GC_GRACE`.
- Public policies control local access; they do not replace Hugging Face license terms or gated-access conditions.

Do not claim Xet or full Transformers compatibility without running a client-version matrix against representative repositories.
