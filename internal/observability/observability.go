// Package observability provides structured logging, request IDs, Prometheus
// metrics, readiness and the admin HTTP mux (/healthz, /readyz, /metrics).
//
// It is the only package that imports the Prometheus client. Other packages
// declare the small Metrics interfaces they need and this package satisfies
// them. Nothing here logs or labels header values, bodies or query strings.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HeaderRequestID is the header carrying the request identifier.
const HeaderRequestID = "X-Request-ID"

// maxRequestIDLength bounds a client-supplied request id. Longer values are
// replaced so a client cannot inflate log lines.
const maxRequestIDLength = 128

// NewLogger returns a slog.Logger writing to stderr at level, as JSON when
// json is true and as text otherwise.
func NewLogger(level slog.Level, json bool) *slog.Logger {
	return newLogger(os.Stderr, level, json)
}

// newLogger is NewLogger with the destination made explicit so tests can
// capture the output. Source locations are off: the message and its
// attributes are enough and the extra cost per line is not worth paying on
// the request path.
func newLogger(w io.Writer, level slog.Level, json bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, AddSource: false}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// requestIDKey is the context key under which the request id travels.
type requestIDKey struct{}

// RequestID ensures every request carries an X-Request-ID, generating one when
// the client did not send one, and echoes it on the response.
//
// A client id is kept when it is at most 128 bytes long and made of
// [A-Za-z0-9._-] only; anything else is replaced with a fresh 16-byte random
// id in hex. The id is set on the inbound request header (so ReverseProxy
// forwards it), on the response header before the next handler writes, and
// in the request context (see RequestIDFrom).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = newRequestID()
		}
		// Set rather than Add so a client sending several values ends up
		// with exactly one, the one we accepted.
		r.Header.Set(HeaderRequestID, id)
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom returns the request id stored by the RequestID middleware, or
// "" when the context does not carry one.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// validRequestID reports whether a client-supplied id is safe to keep: not
// empty, at most maxRequestIDLength bytes, characters in [A-Za-z0-9._-].
func validRequestID(s string) bool {
	if s == "" || len(s) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID returns 16 random bytes encoded as 32 hex characters. Since
// Go 1.24 crypto/rand.Read never returns an error (it crashes the process on
// an unrecoverable failure), so the return values are deliberately ignored.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// routeLabelNone is the route label used when no route matched.
const routeLabelNone = "none"

// durationBuckets covers a proxy's latency range, from 1 ms to 10 s. Anything
// slower than 10 s lands in the +Inf bucket, which is enough to see that
// something is wrong.
var durationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Metrics holds every Prometheus collector sluice exposes, together with the
// registry they are registered on. It satisfies proxy.Metrics and
// controller.Metrics.
//
// Cardinality: labels are the route key (bounded by the number of routes in
// the cluster, hundreds at most), the status code (a few dozen distinct
// values) and the upstream error kind (three fixed values). Host, path,
// headers and upstream addresses are never labels.
type Metrics struct {
	reg *prometheus.Registry

	requests       *prometheus.CounterVec   // sluice_http_requests_total{route,code}
	duration       *prometheus.HistogramVec // sluice_http_request_duration_seconds{route}
	upstreamErrors *prometheus.CounterVec   // sluice_upstream_errors_total{route,kind}

	configVersion    prometheus.Gauge   // sluice_config_version
	configApplied    prometheus.Counter // sluice_config_applied_total
	configRejected   prometheus.Counter // sluice_config_rejected_total
	ingressesSkipped prometheus.Counter // sluice_ingresses_skipped_total
	routes           prometheus.Gauge   // sluice_routes
	ready            prometheus.Gauge   // sluice_ready
}

// NewMetrics creates a private registry, registers the sluice collectors plus
// the standard Go and process collectors on it, and returns the handle.
// Keeping the registry here means no other package imports Prometheus.
func NewMetrics() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_http_requests_total",
			Help: "Requests handled by the proxy, by route key and status code.",
		}, []string{"route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sluice_http_request_duration_seconds",
			Help:    "Time from receiving a request to finishing its response, by route key.",
			Buckets: durationBuckets,
		}, []string{"route"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_upstream_errors_total",
			Help: "Failed upstream exchanges, by route key and kind (dial, timeout, other).",
		}, []string{"route", "kind"}),
		configVersion: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sluice_config_version",
			Help: "Version of the routing table currently served.",
		}),
		configApplied: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sluice_config_applied_total",
			Help: "Configurations compiled and published.",
		}),
		configRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sluice_config_rejected_total",
			Help: "Configurations rejected by validation; the previous table stays in use.",
		}),
		ingressesSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sluice_ingresses_skipped_total",
			Help: "Ingress objects skipped during translation because of an error in them.",
		}),
		routes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sluice_routes",
			Help: "Routes in the routing table currently served.",
		}),
		ready: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sluice_ready",
			Help: "1 when the process reports ready on /readyz, 0 otherwise.",
		}),
	}
	// MustRegister only panics on a duplicate descriptor. The registry is
	// private and the descriptors above are fixed, so a panic here would be
	// a programming error caught by the tests, not a configuration error.
	m.reg.MustRegister(
		m.requests,
		m.duration,
		m.upstreamErrors,
		m.configVersion,
		m.configApplied,
		m.configRejected,
		m.ingressesSkipped,
		m.routes,
		m.ready,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Gatherer exposes the registry for tests and for AdminMux.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// ObserveRequest records one completed request (proxy.Metrics). An empty
// route means no route matched and is labelled "none". The status code is
// labelled as its decimal string.
func (m *Metrics) ObserveRequest(route string, status int, d time.Duration) {
	route = routeLabel(route)
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(route).Observe(d.Seconds())
}

// UpstreamError records a failed upstream exchange (proxy.Metrics). kind is
// one of "dial", "timeout", "other"; an empty kind is counted as "other" so
// the label set stays bounded.
func (m *Metrics) UpstreamError(route, kind string) {
	if kind == "" {
		kind = "other"
	}
	m.upstreamErrors.WithLabelValues(routeLabel(route), kind).Inc()
}

// ConfigApplied records a published configuration (controller.Metrics).
func (m *Metrics) ConfigApplied(version uint64, routes int) {
	m.configVersion.Set(float64(version))
	m.routes.Set(float64(routes))
	m.configApplied.Inc()
}

// ConfigRejected counts a snapshot that failed validation (controller.Metrics).
func (m *Metrics) ConfigRejected() { m.configRejected.Inc() }

// IngressSkipped counts an Ingress skipped during translation (controller.Metrics).
func (m *Metrics) IngressSkipped() { m.ingressesSkipped.Inc() }

// SetReady mirrors the readiness state into the sluice_ready gauge.
func (m *Metrics) SetReady(ready bool) {
	if ready {
		m.ready.Set(1)
		return
	}
	m.ready.Set(0)
}

// routeLabel maps the proxy's "no route" convention to a stable label value.
func routeLabel(route string) string {
	if route == "" {
		return routeLabelNone
	}
	return route
}

// Readiness is a thread-safe readiness flag with an HTTP handler.
type Readiness struct {
	ready atomic.Bool
}

// Set updates the flag.
func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }

// Ready reports the flag.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// Handler answers 200 when ready and 503 otherwise.
func (r *Readiness) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.Ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

// AdminMux serves /healthz (always 200 while the process runs), /readyz and
// /metrics. It is meant for a separate listener, never the proxy port.
func AdminMux(r *Readiness, m *Metrics) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.Handle("/readyz", r.Handler())
	mux.Handle("/metrics", promhttp.HandlerFor(m.Gatherer(), promhttp.HandlerOpts{}))
	return mux
}
