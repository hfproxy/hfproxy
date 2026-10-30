package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	mu                  sync.Mutex
	registry            *prometheus.Registry
	counters            map[string]*prometheus.CounterVec
	counterLabels       map[string][]string
	histograms          map[string]*prometheus.HistogramVec
	histogramLabelNames map[string][]string
	gauges              map[string]prometheus.Gauge
	inFlight            int64
	downloadsInFlight   int64
}

type requestMetricsKey struct{}
type requestMetricsState struct{ cacheDecision sync.Once }

var metricBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 21600}

func newMetrics() *metrics {
	m := &metrics{registry: prometheus.NewRegistry(), counters: map[string]*prometheus.CounterVec{}, counterLabels: map[string][]string{}, histograms: map[string]*prometheus.HistogramVec{}, histogramLabelNames: map[string][]string{}, gauges: map[string]prometheus.Gauge{}}
	for name, labels := range map[string][]string{
		"hfproxy_http_requests_total": {"method", "route", "status_code"}, "hfproxy_http_response_bytes_total": {"route"}, "hfproxy_http_request_bytes_total": {"route"},
		"hfproxy_cache_requests_total": {"result"}, "hfproxy_cache_bytes_served_total": {},
		"hfproxy_upstream_requests_total": {"operation", "status_class"}, "hfproxy_upstream_errors_total": {"reason"}, "hfproxy_upstream_bytes_received_total": {"operation"}, "hfproxy_upstream_retries_total": {"operation"},
		"hfproxy_download_rejected_total": {"reason"}, "hfproxy_downloads_total": {"result"}, "hfproxy_download_bytes_total": {"source"}, "hfproxy_download_failures_total": {"reason"},
		"hfproxy_storage_operations_total": {"operation", "result"}, "hfproxy_storage_errors_total": {"operation"}, "hfproxy_storage_bytes_transferred_total": {"operation"},
		"hfproxy_retention_deleted_total": {"table"}, "hfproxy_cache_evictions_total": {"reason"}, "hfproxy_cache_admissions_total": {"result"},
		"hfproxy_gc_runs_total": {"result"}, "hfproxy_gc_blobs_deleted_total": {}, "hfproxy_cache_object_repairs_total": {}, "hfproxy_published_bytes_total": {}, "hfproxy_published_commits_total": {}, "hfproxy_unsafe_format_refused_total": {"where"},
		"hfproxy_api_cache_requests_total": {"result"}, "hfproxy_upstream_breaker_opened_total": {"host"}, "hfproxy_upstream_breaker_rejected_total": {"host"},
	} {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: name + " measured by hfproxy."}, labels)
		m.registry.MustRegister(v)
		m.counters[name] = v
		m.counterLabels[name] = labels
	}
	for name, labels := range map[string][]string{"hfproxy_http_request_duration_seconds": {"method", "route"}, "hfproxy_cache_lookup_duration_seconds": {}, "hfproxy_upstream_request_duration_seconds": {"operation"}, "hfproxy_download_duration_seconds": {"result"}, "hfproxy_download_ttfb_seconds": {"cache"}, "hfproxy_storage_operation_duration_seconds": {"operation"}} {
		v := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: name + " measured by hfproxy.", Buckets: metricBuckets}, labels)
		m.registry.MustRegister(v)
		m.histograms[name] = v
		m.histogramLabelNames[name] = labels
	}
	for _, name := range []string{"hfproxy_http_requests_in_flight", "hfproxy_downloads_in_flight", "hfproxy_cache_objects", "hfproxy_cache_size_bytes", "hfproxy_gc_last_run_timestamp_seconds", "hfproxy_cache_pinned_bytes", "hfproxy_staging_free_bytes", "hfproxy_staging_reserved_bytes"} {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: name + " measured by hfproxy."})
		m.registry.MustRegister(g)
		m.gauges[name] = g
	}
	// Materialize bounded zero-valued series for dashboard operations and results.
	for _, result := range []string{"hit", "miss", "bypass", "error"} {
		m.counter("hfproxy_cache_requests_total", []string{"result=" + result}).Add(0)
	}
	for _, op := range []string{"metadata", "resolve", "download", "other"} {
		m.counter("hfproxy_upstream_retries_total", []string{"operation=" + op}).Add(0)
		m.counter("hfproxy_upstream_bytes_received_total", []string{"operation=" + op}).Add(0)
		for _, status := range []string{"2xx", "3xx", "4xx", "5xx", "error"} {
			m.counter("hfproxy_upstream_requests_total", []string{"operation=" + op, "status_class=" + status}).Add(0)
		}
	}
	for _, reason := range []string{"timeout", "connection", "cancelled", "other", "http_4xx", "http_5xx"} {
		m.counter("hfproxy_upstream_errors_total", []string{"reason=" + reason}).Add(0)
	}
	for _, op := range []string{"read", "write", "delete", "metadata"} {
		for _, result := range []string{"success", "error"} {
			m.counter("hfproxy_storage_operations_total", []string{"operation=" + op, "result=" + result}).Add(0)
		}
		m.counter("hfproxy_storage_errors_total", []string{"operation=" + op}).Add(0)
		m.counter("hfproxy_storage_bytes_transferred_total", []string{"operation=" + op}).Add(0)
	}
	for _, result := range []string{"success", "failed", "cancelled", "partial"} {
		m.counter("hfproxy_downloads_total", []string{"result=" + result}).Add(0)
	}
	for _, reason := range []string{"http_error", "client_disconnect", "cancelled"} {
		m.counter("hfproxy_download_failures_total", []string{"reason=" + reason}).Add(0)
	}
	for _, source := range []string{"cache", "upstream"} {
		m.counter("hfproxy_download_bytes_total", []string{"source=" + source}).Add(0)
	}
	m.counter("hfproxy_cache_bytes_served_total", nil).Add(0)
	return m
}

func labelValues(input, names []string) []string {
	values := make(map[string]string, len(input))
	for _, v := range input {
		p := strings.SplitN(v, "=", 2)
		if len(p) == 2 {
			values[p[0]] = p[1]
		}
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = values[name]
	}
	return out
}
func (m *metrics) counter(name string, labels []string) prometheus.Counter {
	v := m.counters[name]
	return v.WithLabelValues(labelValues(labels, m.counterLabels[name])...)
}
func (m *metrics) add(name string, labels []string, value float64) {
	if v := m.counters[name]; v != nil {
		m.counter(name, labels).Add(value)
	}
}
func (m *metrics) observe(name string, labels []string, value float64) {
	if v := m.histograms[name]; v != nil {
		v.WithLabelValues(labelValues(labels, m.histogramLabelNames[name])...).Observe(value)
	}
}
func (m *metrics) setGauge(name string, value float64) {
	if g := m.gauges[name]; g != nil {
		g.Set(value)
	}
}
func (m *metrics) gaugeAdd(name string, value float64) {
	if g := m.gauges[name]; g != nil {
		g.Add(value)
	}
}
func (m *metrics) ObserveStorage(operation, result string, durationSeconds float64, bytes int64) {
	m.add("hfproxy_storage_operations_total", []string{"operation=" + operation, "result=" + result}, 1)
	m.observe("hfproxy_storage_operation_duration_seconds", []string{"operation=" + operation}, durationSeconds)
	if result != "success" {
		m.add("hfproxy_storage_errors_total", []string{"operation=" + operation}, 1)
	}
	if bytes > 0 {
		m.add("hfproxy_storage_bytes_transferred_total", []string{"operation=" + operation}, float64(bytes))
	}
}

func routeTemplate(path string) string {
	switch {
	case path == "/":
		return "/"
	case strings.HasPrefix(path, "/api/v1/"):
		return "/api/v1/{path}"
	case strings.HasPrefix(path, "/api/"):
		return "/api/{path}"
	case path == "/healthz" || path == "/readyz" || path == "/metrics" || path == "/login" || path == "/logout" || path == "/auth/login" || path == "/auth/callback" || path == "/assets/app.css":
		return path
	case strings.HasPrefix(path, "/models/"):
		return "/models/{repo}/resolve/{revision}/{file}"
	case strings.HasPrefix(path, "/datasets/"):
		return "/datasets/{repo}/resolve/{revision}/{file}"
	case strings.HasPrefix(path, "/spaces/"):
		return "/spaces/{repo}/resolve/{revision}/{file}"
	default:
		return "/{path}"
	}
}

type measuredWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	writeErr error
	started  time.Time
	// firstByte is the delay until the response headers were sent, and cache the
	// X-Cache value they carried: together they give the time to first byte.
	firstByte time.Duration
	cache     string
}

type measuredRequestBody struct {
	io.ReadCloser
	bytes atomic.Int64
}

func (b *measuredRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes.Add(int64(n))
	return n, err
}

func (w *measuredWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		if !w.started.IsZero() {
			w.firstByte = time.Since(w.started)
		}
		w.cache = w.ResponseWriter.Header().Get("X-Cache")
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *measuredWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}
func (w *measuredWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *measuredWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
func (w *measuredWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
func (w *measuredWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := io.Copy(writerOnly{w.ResponseWriter}, r)
	w.bytes += n
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

type writerOnly struct{ io.Writer }

func (m *metrics) middleware(metricsPath string, next http.Handler) http.Handler {
	return m.middlewareDynamic(func() string { return metricsPath }, next)
}
func (m *metrics) middlewareDynamic(metricsPath func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := routeTemplate(r.URL.Path)
		if p := metricsPath(); p != "" && r.URL.Path == p {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		r = r.WithContext(context.WithValue(r.Context(), requestMetricsKey{}, &requestMetricsState{}))
		var requestBody *measuredRequestBody
		if r.Body != nil && r.Body != http.NoBody {
			requestBody = &measuredRequestBody{ReadCloser: r.Body}
			r.Body = requestBody
		}
		mw := &measuredWriter{ResponseWriter: w, started: start}
		download := r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/resolve/")
		m.mu.Lock()
		m.inFlight++
		m.gaugeAdd("hfproxy_http_requests_in_flight", 1)
		if download {
			m.downloadsInFlight++
			m.gaugeAdd("hfproxy_downloads_in_flight", 1)
		}
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.inFlight--
			m.gaugeAdd("hfproxy_http_requests_in_flight", -1)
			if download {
				m.downloadsInFlight--
				m.gaugeAdd("hfproxy_downloads_in_flight", -1)
			}
			m.mu.Unlock()
			status := mw.status
			if status == 0 {
				status = 200
			}
			m.add("hfproxy_http_requests_total", []string{"method=" + r.Method, "route=" + route, "status_code=" + strconv.Itoa(status)}, 1)
			m.observe("hfproxy_http_request_duration_seconds", []string{"method=" + r.Method, "route=" + route}, time.Since(start).Seconds())
			m.add("hfproxy_http_response_bytes_total", []string{"route=" + route}, float64(mw.bytes))
			if requestBody != nil {
				m.add("hfproxy_http_request_bytes_total", []string{"route=" + route}, float64(requestBody.bytes.Load()))
			}
			if download {
				result := "success"
				reason := ""
				if r.Context().Err() != nil {
					result = "cancelled"
					reason = "cancelled"
				} else if mw.writeErr != nil {
					result = "failed"
					reason = "client_disconnect"
				} else if status >= 400 {
					result = "failed"
					reason = "http_error"
				} else if status == http.StatusPartialContent || r.Header.Get("Range") != "" {
					result = "partial"
				}
				m.add("hfproxy_downloads_total", []string{"result=" + result}, 1)
				if status < 400 && r.Context().Err() == nil {
					cache := strings.ToLower(mw.cache)
					if cache != "hit" && cache != "miss" && cache != "stale" {
						cache = "other"
					}
					m.observe("hfproxy_download_ttfb_seconds", []string{"cache=" + cache}, mw.firstByte.Seconds())
				}
				m.observe("hfproxy_download_duration_seconds", []string{"result=" + result}, time.Since(start).Seconds())
				source := "upstream"
				if mw.Header().Get("X-Cache") == "HIT" {
					source = "cache"
				}
				m.add("hfproxy_download_bytes_total", []string{"source=" + source}, float64(mw.bytes))
				if reason != "" {
					m.add("hfproxy_download_failures_total", []string{"reason=" + reason}, 1)
				}
			}
		}()
		next.ServeHTTP(mw, r)
	})
}
func (m *metrics) cacheDecision(ctx context.Context, result string) {
	state, ok := ctx.Value(requestMetricsKey{}).(*requestMetricsState)
	if ok {
		state.cacheDecision.Do(func() { m.add("hfproxy_cache_requests_total", []string{"result=" + result}, 1) })
	}
}
func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: false})
}
