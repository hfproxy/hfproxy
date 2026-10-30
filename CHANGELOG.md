# Changelog

All notable changes to hfproxy. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [semantic versioning](https://semver.org/) (it is below 1.0: minor versions may change behaviour, and say so here). The notes of a release are taken from `docs/releases/VERSION.md` when that file exists, otherwise from the section of this file; see [docs/releasing.md](docs/releasing.md).

## [Unreleased]

### Added

- **Two more console languages.** English pirate and English emoji join the language selector.
- **Safe formats only.** A new setting (`HFPROXY_SAFE_FORMATS_ONLY`, or Settings → Cache rules) stops the proxy from serving, preloading or accepting files that can run code when a client loads them: `.bin`, `.pt`, `.pth`, `.ckpt`, `.pkl`, `.pickle`, `.joblib` and `.pdparams`. A refused read is a 404 `EntryNotFound`, so clients that also find a safetensors file use it; a refused upload is a 422 with the reason; a preload skips the file and counts it. A published file named `.safetensors` must have a valid header, so a pickle cannot be renamed. An administrator can exempt one repository (migration 17). See [docs/safe-formats.md](docs/safe-formats.md).
- **Publishing models and datasets on the proxy.** Users can create, fill, read and delete their own models and datasets with `huggingface_hub`, the `hf` command or the console, under their name or a group's, public or private, with branches, tags, history, Git LFS for large files and model cards. Nothing is sent to Hugging Face. It has a storage quota per user and a file size limit, a `publish` token scope, and a *Models & datasets* page with upload, folders, README rendering and history. Commits store only what changed, with periodic snapshots. See [docs/publishing.md](docs/publishing.md). A user and a group can no longer share a name (migrations 16).
- **First administrator from the console.** A new instance offers a setup page; the setup code is printed at start (or fixed with `HFPROXY_SETUP_TOKEN`), and the page closes once an administrator exists.
- **API and download-path logging with a level that can be changed at run time** (`HFPROXY_LOG_LEVEL`, or Settings → Observability). Errors carry their cause, requests are logged, administrative actions are audited in the log, and the download path logs hits, Hub answers and failures.
- **Download limits**, all off by default and editable in the console: simultaneous downloads per user (or per address), speed per transfer, and a daily quota per user, with a per-user override. A refusal is a `429` with `Retry-After` and is counted in `hfproxy_download_rejected_total`.
- **Token revocation reaches every replica at once** through PostgreSQL notifications (`HFPROXY_AUTH_CACHE_TTL` stays the upper bound).
- **`HFPROXY_HF_ALLOWED_PRIVATE_CIDRS`** to reach a Hub mirror on a private network. Only private ranges are accepted; link-local and loopback can never be allowed.
- **`hfproxy_download_ttfb_seconds{cache}`**, the time to first byte of successful downloads, for latency objectives.
- **Operations documentation**: a runbook per alert, service level objectives with burn-rate alert rules, a sizing guide, a rotation guide for every credential and certificate, and a second Grafana dashboard (resilience and limits). Every alert has a `runbook_url`.
- **A test stack** (`docker-compose.test.yml`) with Keycloak, Prometheus and Grafana.
- **Validation protocols and tools**: `scripts/large-file-test.py` (the real binary with a multi-gigabyte file, reporting memory and staging use), `scripts/backup-restore-drill.sh` (a restore rehearsed on a copy, timed) and [docs/validation.md](docs/validation.md) for a real cluster, the real Hub and a large model.
- **Release pipeline**: a release is created from the tag with binaries for linux/amd64 and linux/arm64, a CycloneDX SBOM, the Helm chart, signed checksums and the notes from this file; the chart is also published as an OCI artifact. The image is checked for bit-for-bit reproducibility in CI.

### Changed

- **A repository grant can only give `read`.** `write` and `admin` were accepted but enforced as `read`, which looked like a feature. The API refuses them, the console no longer offers them, and migration 14 turns existing ones into `read`.
- **The hit and miss totals of the console are counters kept per repository** (migration 15, filled from the downloads table). Reading them no longer scans the downloads table, and they no longer shrink when old download records are pruned.
- **LDAP accounts are found by their stable identifier, not by the directory's address.** Changing `ldap://` for `ldaps://`, renaming the server or changing its port used to make every user a stranger (duplicates with auto-provisioning, no sign-in without it). The recorded address now follows the configured one.
- **Connections to the OIDC issuer and the LDAP server are filtered**: cloud metadata services, link-local, unspecified and multicast addresses (and loopback outside development) are refused on the address actually connected to. OIDC calls now have a timeout and follow at most three redirects. Private addresses stay allowed.
- **Dashboard statistics are kept for 15 seconds** per viewer, and computed once for simultaneous requests: the console asked at every page.
- A failed Hub request now logs why (without the URL, which can carry a signature).
- The Helm chart's `appVersion` is kept in line with the latest release.

### Fixed

- **The console no longer polls every two seconds when nothing is happening.** The overview and repositories pages asked for the active downloads at that pace forever. They now do so only while a download or a preload is in progress; the overview checks every 15 seconds otherwise, the repositories page only when the tab is shown again, and a hidden tab does not poll.
- **Clicking Preload shows the new attempt at once**, not the previous failure and its error until the next poll.
- **A cached file whose object vanished from the bucket repairs itself.** The request that finds the object missing drops the catalog entry and fetches the file again (no more `502` until an operator purges it); the repair is logged and counted in `hfproxy_cache_object_repairs_total`.
- **A purge could deadlock with a fill of the same file.** The purge (console and `hfproxy cache purge`) locked the catalog entry then the blob, a fill the other way round; PostgreSQL ended one of them, sometimes a fill after hours of download. Both now lock the blob first. The CLI purge shares the code of the console's.
- **The cleanup of abandoned resumptions never respected a fill in progress.** It used another lock key than the fill, so a fill stuck on a dead connection could have its stored blocks deleted under it after seven days. They now share one key.
- **Saving the settings overwrote other people's changes.** Two administrators, or a replica that had not seen a change, silently replaced each other's settings, secrets included. The console now sends the revision it read, a save on an outdated one is refused (`409`), and an instance adopts the saved settings when the console opens them.
- **Waiting for another instance's fill no longer blocks a slot for hours.** The wait for the file's cluster-wide lock is bounded to one minute; after that the file is streamed from the Hub, or the client is told to retry (`503`, `Retry-After`).
- **`hfproxy admin reset-password` now revokes the administrator's access tokens**, not only their web sessions: a stolen token no longer survives the reset.
- **Signing out reports a failure** (`500`) when the session could not be ended, instead of claiming success while the session stays valid.
- **After signing out, signing in again as another user no longer shows the previous page**, and an error banner no longer outlives the session that caused it.
- **One `Secure` rule for every cookie** (https public URL or https OIDC redirect): the CSRF cookie could be unprotected next to a Secure session cookie.
- **A revoked credential could be cached again** by a lookup that began before the revocation; it is now ignored.
- The cache refuses a `HFPROXY_CACHE_MAX_FILE_SIZE` above 625 GiB (10000 parts of 64 MiB, the object store's limit) instead of failing after hours of transfer.
- Finished preload jobs are forgotten after 24 hours; the negative cache no longer scans all its entries on every insert; the admission of downloads no longer waits for a database query when its quota answer expires; stopping no longer spends minutes writing queued download events when the database is down (15 s at most, the rest is reported).

- **A file could be listed in the cache whose object was gone for good.** If a file was downloaded again at the moment the hourly collector deleted its orphaned blob, the new catalog entry could be recorded without the object being restored, and every later download of that file answered 502 until the entry was purged by hand. The object is now checked and restored when the download did not upload it itself. Present in 0.1.0; the window is a few milliseconds per orphaned blob and collection pass, so it is rare. If you suspect it, `hfproxy cache verify` lists the affected files and `hfproxy cache purge` drops their entries.
- The log line for a cached object that cannot be read now says why, and whether the object is missing.
- **A clean stop exited with code 1.** After a successful drain `hfproxy serve` reported `http: Server closed` as an error, so every rolling update looked like a crash to a supervisor; a stop that had to cut long downloads did the same. A stop now exits 0 and logs how many requests were cut. The grace is configurable with `HFPROXY_SHUTDOWN_GRACE` (default 30s) and the chart derives it from `terminationGracePeriodSeconds`.
- **A race in the download limiter could crash the instance.** When a user reached their simultaneous-download limit at the moment another of their downloads finished, the refusal read a map without its lock, which Go treats as a fatal error that takes down every stream of the instance. Introduced with the download limits (unreleased), never in a release.
- Documentation said a Hub mirror "works" without mentioning that private addresses were refused; it now describes the option above.

### Tests and CI

- The chart is installed in a real (kind) cluster and exercised: migration job, first start, cached small and large downloads, a range request, a rolling update to two replicas and a replica killed during use.
- A replica killed in the middle of a cache fill: the interrupted client gets an error and not a corrupted file, and the other replica completes the file.
- The LDAP sign-in is tested against a real OpenLDAP server and the OIDC sign-in against a real Keycloak.

## [0.1.0] - 2026-10-29

A production-hardening release: seven languages for the console, resilient Hub access (retries, circuit breaker, stale-if-error), cache size limit with eviction and pins, stable LDAP identities and group synchronisation, a stricter Content-Security-Policy and key derivation, a Helm chart and a distroless image. Behaviour changes to check before upgrading (public `/metrics` now requires an operator, restricted outbound ports, no default secrets in Compose, new Go module path) are listed in [docs/releases/0.1.0.md](docs/releases/0.1.0.md).

## [0.0.4]

Earlier development versions. See the git history.

[Unreleased]: https://github.com/hfproxy/hfproxy/compare/0.1.0...HEAD
[0.1.0]: https://github.com/hfproxy/hfproxy/compare/0.0.4...0.1.0
[0.0.4]: https://github.com/hfproxy/hfproxy/releases/tag/0.0.4
