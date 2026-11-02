# hfproxy

<p align="center">
  <img src="ui/public/hfproxy-logo.svg" alt="hfproxy — Hugging Face Proxy" width="250">
</p>

<p align="center">
  <a href="https://github.com/hfproxy/hfproxy/actions/workflows/ci.yml"><img src="https://github.com/hfproxy/hfproxy/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI status: formatting, vet, tests, and build"></a>
  <a href="https://github.com/hfproxy/hfproxy/releases/latest"><img src="https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fapi.github.com%2Frepos%2Fhfproxy%2Fhfproxy%2Freleases%2Flatest&amp;query=%24.tag_name&amp;label=version&amp;color=blue" alt="Latest published version"></a>
  <a href="https://github.com/hfproxy/hfproxy/pkgs/container/hfproxy"><img src="https://img.shields.io/badge/container-GHCR-2496ED?logo=docker&amp;logoColor=white" alt="Container image on GitHub Container Registry"></a>
  <img src="https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue?logo=docker&amp;logoColor=white" alt="Supported container platforms: amd64 and arm64">
</p>


**A private Hugging Face Hub for your organization.** hfproxy sits between your teams and the Hub: it caches models and datasets once in your own S3 bucket, decides who may read what, and lets your users publish models and datasets of their own, all through the Hugging Face tools they already use. Point `HF_ENDPOINT` at it and nothing else changes.

<table>
  <tr>
    <td width="50%" align="center"><img src="images/overview.png" alt="hfproxy administration overview showing repository, cache, storage, and hit-rate metrics" width="100%"></td>
    <td width="50%" align="center"><img src="images/repository.png" alt="hfproxy repository administration showing policies, cache status, and preload controls" width="100%"></td>
  </tr>
  <tr>
    <td align="center"><sub>Overview: cache and service health at a glance</sub></td>
    <td align="center"><sub>Repositories: access policies and preload status</sub></td>
  </tr>
</table>

## Why hfproxy

- **Download once, serve everyone.** Files are fetched from the Hub on first use, deduplicated by SHA-256 and kept in your S3-compatible bucket. Later reads never leave your network.
- **You decide who reads what.** Every request is checked against a policy before any byte is served, cache hits included: `admin_only`, public, or an allowlist of users and groups.
- **Publish your own models and datasets.** Users upload to the proxy with `huggingface_hub` or the console, under their name or a group's, public or private. Nothing is sent to Hugging Face.
- **No client changes.** `from_pretrained`, `hf_hub_download`, `snapshot_download` and `datasets` work unchanged with `HF_ENDPOINT` set.
- **Survives a bad day at the Hub.** Circuit breaker, retries with backoff, resumable downloads, and stale cache answers when the Hub is down.
- **Built to be operated.** Prometheus metrics, SLO rules, Grafana dashboards, runbooks, audit log, and a Helm chart.

## Features

| | |
| --- | --- |
| **Caching** | SHA-256 deduplication, branch/tag/SHA revisions, ETag/304, single Range requests, resumable 64 MiB-block fills, [five eviction strategies, limits per repository and organization, and an admission filter](docs/cache-strategies.md), pinning of revisions that must never be evicted, preload from the console, integrity verification (`cache verify`). |
| **Access control** | Per-repository policies, local users and groups, roles (`admin`, `operator`, `reader`), hashed personal tokens with `read`, `publish`, `write` and `admin` scopes, OIDC/SSO, LDAP/LDAPS with group sync, anonymous reads for public repositories only. |
| **Publishing** | Models and datasets on the proxy, with branches, tags, history, Git LFS for large files, model cards, per-user quota and file size limit. See [Publishing](docs/publishing.md). |
| **Safe formats** | Optionally refuse pickle-based weights (`.bin`, `.pt`, `.ckpt`...) on read, preload and publish, check that `.safetensors` files really are, and exempt a repository when needed. See [Safe formats](docs/safe-formats.md). |
| **Console** | Angular interface for repositories and grants, cache, users, groups, tokens, audit, live settings and published repositories. In English, French, Spanish, German, Portuguese, Chinese and Japanese. |
| **Resilience** | Circuit breaker per Hub host, in-memory metadata cache with stale answers, download limits (concurrency, speed, daily quota), staging-disk protection, graceful shutdown. |
| **Operations** | Prometheus metrics, OpenTelemetry tracing, JSON logs with request IDs, `/healthz` and `/readyz`, runtime log level, backup and restore guide, several replicas behind a load balancer. |
| **Security** | Secrets encrypted in PostgreSQL (AES-GCM), keyed token hashes, CSRF on browser mutations, no generic URL proxy, redirect allowlist, rate-limited sign-in, CI with `gosec`, `staticcheck`, `govulncheck` and image scanning, reproducible builds. See the [threat model](docs/threat-model.md). |

## Quick start

The Compose setup is for development on a trusted machine (demo passwords, plain HTTP). For anything else, read [Running in production](docs/production.md).

```bash
cp .env.example .env
docker compose up --build -d
```

1. **Create the first administrator.** Open http://localhost:8080, read the setup code printed at start (`docker compose logs hfproxy`) and fill in the form. See [First start](docs/operations.md#first-start).
2. **Register a repository** and choose its policy (`public`, `admin_only` or an allowlist), from the console or:

   ```bash
   docker compose exec hfproxy hfproxy admin repo add --type models --id bert-base-uncased --policy public
   ```

3. **Create a token.** In **Access tokens**, pick an expiry and copy the secret (shown once). Or from a shell:

   ```bash
   docker compose exec hfproxy hfproxy admin token create --username admin --name laptop
   ```

4. **Use it.**

   ```bash
   export HF_ENDPOINT=http://localhost:8080
   export HF_TOKEN=hfpx_your_hfproxy_token
   python -c 'from huggingface_hub import hf_hub_download; print(hf_hub_download("bert-base-uncased", "config.json"))'
   ```

The first read fetches from the Hub; the second is served from your bucket.

### Publish your own model

Create a token with the **publish** scope, then:

```python
from huggingface_hub import HfApi

api = HfApi(endpoint="http://localhost:8080", token="hfpx_your_publish_token")
api.create_repo("alice/my-model", private=True)
api.upload_folder(folder_path="./my-model", repo_id="alice/my-model")
```

Datasets work the same with `repo_type="dataset"`. With `huggingface_hub` 1.x or later, set `HF_HUB_DISABLE_XET=1` when uploading (the proxy does not speak Xet; it falls back to Git LFS). Who can publish where, visibility, limits and storage are in [Publishing](docs/publishing.md).

## How it works

    client ──HF_ENDPOINT──▶ hfproxy ──▶ policy check ──┬─▶ S3 bucket (blobs, deduplicated by SHA-256)
                              │                         └─▶ Hugging Face Hub (only on a cache miss)
                              └──▶ PostgreSQL (catalog, users, policies, audit)

- Hub repositories are fetched on demand and cached. Repositories published on the proxy live next to them in the same bucket and catalog, are served by the same API, and are never evicted.
- Authorization comes first on every path. The upstream token stays on the server and is never sent to a client.
- PostgreSQL and an S3-compatible bucket are required. Back both up together.

## Compatibility with the Hugging Face Hub

Read routes: `resolve` for models, datasets and spaces files; repository info, revisions, tree, commits; `whoami-v2`. Write routes, for repositories published on the proxy only: create, delete, settings, `preupload`, NDJSON `commit`, Git LFS (`basic` transfer), branches and tags. Tested end to end with `huggingface_hub` 0.30.2 and 2.4.0 in CI.

Not supported: Xet/CAS, `git` over HTTP, pull requests and discussions, inference, Spaces execution. The Hub itself is never written to. The route-by-route matrix, security guarantees and test evidence are in [docs/huggingface-compatibility.md](docs/huggingface-compatibility.md).

## Configuration

Settings come from `HFPROXY_*` environment variables and, once saved, from the console, which takes precedence (lock a setting to the environment with `HFPROXY_LOCKED_SETTINGS`). The essentials:

| Setting | Purpose |
| --- | --- |
| `HFPROXY_DATABASE_URL` | PostgreSQL connection |
| `HFPROXY_SESSION_SECRET` | Protects sessions, tokens and saved settings (see [rotation](docs/operations.md#rotating-the-session-secret)) |
| `HFPROXY_PUBLIC_URL` | Public address, `https://` in production |
| `HFPROXY_S3_*` | Endpoint, bucket, region, credentials |
| `HFPROXY_HF_TOKEN` | Upstream token for private or gated repositories (optional) |
| `HFPROXY_CACHE_MAX_TOTAL_SIZE` | Cache quota, least recently used files are evicted |
| `HFPROXY_CUSTOM_REPOSITORIES` | Turn publishing on or off |

The complete list, OIDC and LDAP setup, command line, pinning, resilience and container deployment are in [docs/configuration.md](docs/configuration.md).

```text
hfproxy serve | migrate | admin | cache | config | version
```

## Documentation

| For | Read |
| --- | --- |
| Running it | [Production checklist](docs/production.md), [Operations](docs/operations.md), [Sizing](docs/sizing.md), [Runbooks](docs/runbooks.md) |
| Configuring it | [Configuration reference](docs/configuration.md), [Publishing](docs/publishing.md) |
| Watching it | [Observability](docs/observability.md), [SLOs](docs/slo.md), Grafana and Prometheus files in `docs/` |
| Cache behavior | [Cache strategies](docs/cache-strategies.md) |
| Trusting it | [Safe formats](docs/safe-formats.md), [Threat model](docs/threat-model.md), [Security scanning](docs/security-scanning.md), [SECURITY.md](SECURITY.md) |
| Hub compatibility | [Compatibility audit](docs/huggingface-compatibility.md), [Validation](docs/validation.md) |
| Trying it | [Testing stack with Keycloak, Prometheus and Grafana](docs/testing-stack.md) |
| Contributing | [Releasing](docs/releasing.md), [Changelog](CHANGELOG.md), [Frontend and translations](ui/README.md) |

## Deploying

Images for `linux/amd64` and `linux/arm64` are published to GitHub Container Registry on every version tag, built from a reproducible pipeline and running as a non-root user:

```bash
docker pull ghcr.io/hfproxy/hfproxy:0.0.4
```

Pin a version in production. A Helm chart is in `deploy/helm/hfproxy`. `docker-compose.example.yml` shows secrets through `*_FILE`, PostgreSQL with `verify-full` and TLS behind a reverse proxy.

## Current limitations

- No Xet/CAS, `git` over HTTP, pull requests, inference or Spaces execution. Uploads exist only for repositories published on the proxy.
- OIDC group claims are not mapped to local groups (LDAP groups can be synced).
- Rate limits are per process, not distributed. Multi-part Range requests are rejected.
- Public policies control local access; they do not replace Hugging Face license terms or gated-access conditions.
- Do not assume full Transformers compatibility for a given client version without trying it on your repositories; see [Validation](docs/validation.md).

## License

MIT, see [LICENSE](LICENSE).
