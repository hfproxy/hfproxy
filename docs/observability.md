# Observability

hfproxy uses Prometheus metrics and OpenTelemetry traces. Metrics are the primary metrics integration. Tracing can be enabled independently and exported using OTLP over HTTP/protobuf.

## Configuration

Metrics are disabled by default. Enable them with `HFPROXY_METRICS_ENABLED=true`; the default path is `/metrics`. Tracing is disabled by default. Configure `HFPROXY_TRACING_ENABLED=true`, `HFPROXY_OTLP_ENDPOINT=http://otel-collector:4318/v1/traces`, `HFPROXY_TRACING_SERVICE_NAME=hfproxy`, and `HFPROXY_TRACING_SAMPLING_RATIO=0.1` to sample about one in ten root traces. The sampling ratio must be between 0 and 1. W3C Trace Context is extracted from incoming requests and propagated to Hugging Face upstream requests. Sampling is parent based.

Admins can edit these settings in **Settings → Observability**. Changes are encrypted with the existing settings store and applied at runtime. The OTLP endpoint must be an HTTP(S) URL without user information or query parameters. Keep the Prometheus endpoint on a private network: it bypasses application authentication. It is excluded from request metrics and trace spans.

## Metrics

HTTP route labels are normalized templates. No metric labels contain repository IDs, filenames, tokens, user IDs, upstream URLs, signed query parameters, or storage keys.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hfproxy_http_requests_total` | Counter | method, route, status_code | Completed HTTP requests |
| `hfproxy_http_request_duration_seconds` | Histogram | method, route | Full request duration, buckets 5ms–6h |
| `hfproxy_http_requests_in_flight` | Gauge | — | Active HTTP requests |
| `hfproxy_http_response_bytes_total` | Counter | route | Bytes accepted by the response writer, including streams and ranges |
| `hfproxy_http_request_bytes_total` | Counter | route | Request body bytes read by handlers |
| `hfproxy_cache_requests_total` | Counter | result | One cache decision per HTTP request: hit, miss, bypass, error |
| `hfproxy_cache_evictions_total` | Counter | reason | Files evicted, by the limit that caused it: idle, revisions, repository, namespace, ring, total |
| `hfproxy_cache_admissions_total` | Counter | result | Cache misses admitted or refused by the [admission policy](cache-strategies.md#admission) |
| `hfproxy_cache_lookup_duration_seconds` | Histogram | — | Cache metadata lookup time |
| `hfproxy_cache_bytes_served_total` | Counter | — | Bytes copied from cache objects to clients |
| `hfproxy_cache_object_repairs_total` | Counter | — | Cached files whose object was missing from the bucket and were fetched again |
| `hfproxy_published_bytes_total` | Counter | — | Bytes received for new content in published repositories |
| `hfproxy_published_commits_total` | Counter | — | Commits recorded in published repositories |
| `hfproxy_unsafe_format_refused_total` | Counter | where | Files refused by the safe-formats setting: `read` (a client asked for one), `preload` (skipped), `publish` (an upload refused) |
| `hfproxy_cache_objects` | Gauge | — | Blob object count from database metadata |
| `hfproxy_cache_size_bytes` | Gauge | — | Blob size sum from database metadata |
| `hfproxy_upstream_requests_total` | Counter | operation, status_class | Actual upstream HTTP attempts; operations are metadata, resolve, download, other |
| `hfproxy_upstream_request_duration_seconds` | Histogram | operation | Time to receive upstream response headers |
| `hfproxy_upstream_errors_total` | Counter | reason | Network, timeout, cancellation, and HTTP class failures |
| `hfproxy_upstream_bytes_received_total` | Counter | operation | Bytes read from upstream response bodies |
| `hfproxy_upstream_retries_total` | Counter | operation | Retries (hfproxy currently does not retry requests) |
| `hfproxy_downloads_total` | Counter | result | Client GET sessions for Hub resolve paths; success, failed, cancelled, partial |
| `hfproxy_download_duration_seconds` | Histogram | result | Client download session duration |
| `hfproxy_download_ttfb_seconds` | Histogram | cache | Time from the request to the first response byte, for successful downloads; `cache` is `hit`, `miss`, `stale` or `other` (the `X-Cache` header). The latency indicator in [slo.md](slo.md) |
| `hfproxy_download_bytes_total` | Counter | source | Bytes written to clients, source cache or upstream |
| `hfproxy_downloads_in_flight` | Gauge | — | Active resolve GET sessions |
| `hfproxy_download_failures_total` | Counter | reason | `http_error`, `client_disconnect`, or `cancelled` client sessions |
| `hfproxy_download_rejected_total` | Counter | reason | Downloads refused by a limit: `concurrency` or `quota` (see [Download limits](operations.md#download-limits)) |
| `hfproxy_storage_operations_total` | Counter | operation, result | S3 read, write, delete, and metadata operations |
| `hfproxy_storage_operation_duration_seconds` | Histogram | operation | S3 operation duration |
| `hfproxy_storage_errors_total` | Counter | operation | Failed S3 operations |
| `hfproxy_storage_bytes_transferred_total` | Counter | operation | Bytes read from S3 and bytes consumed by the S3 upload client |

Cache inventory gauges refresh asynchronously using database metadata, every five minutes by default. `HFPROXY_CACHE_INVENTORY_REFRESH` configures the interval; admins can change it in the Observability settings. Scrapes never scan S3 or query the database.

Download terminology:

1. A **client download session** is one GET request to a Hub resolve path. Ranges and cancelled connections are recorded as partial or cancelled sessions.
2. An **upstream artifact fetch** is an outbound Hub/CDN HTTP attempt, including each redirect hop. It is measured separately from client sessions.
3. A **cache-to-client transfer** is counted once at the streaming copy from an S3 object to the response writer. Upstream bytes are measured where the upstream body is read.

`hfproxy_downloads_total` is a request/session count, not a count of complete artifacts. A 206 or request with a Range header is `partial` and is not counted as a completed download.

## Kubernetes and Grafana

Expose the metrics path only through a private Service used by Prometheus. Example ServiceMonitor:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: hfproxy
spec:
  selector:
    matchLabels:
      app: hfproxy
  endpoints:
    - port: http
      path: /metrics
```

Import [`grafana-observability.json`](grafana-observability.json) and select the Prometheus datasource variable after import. Example PromQL:

```promql
sum(rate(hfproxy_http_requests_total[5m]))
histogram_quantile(0.95, sum by (le) (rate(hfproxy_http_request_duration_seconds_bucket[5m])))
sum(rate(hfproxy_cache_requests_total{result="hit"}[5m])) / clamp_min(sum(rate(hfproxy_cache_requests_total{result=~"hit|miss"}[5m])), 1e-9)
sum(rate(hfproxy_upstream_errors_total[5m]))
sum(rate(hfproxy_download_bytes_total[5m]))
```

## Trace data and privacy

Spans cover incoming HTTP requests, cache lookups, Hugging Face upstream attempts, and S3 operations. Span attributes are limited to route templates, HTTP method/status, bounded operation names, and bounded error classes. Authorization headers, tokens, full URLs, signed query strings, repository identifiers, filenames, and S3 object keys are not recorded. OTLP exporter credentials should be provided by an in-cluster collector rather than embedded in the endpoint URL.
