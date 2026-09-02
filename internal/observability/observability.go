// Package observability provides structured logging, request IDs, Prometheus
// metrics, readiness and the admin HTTP mux (/healthz, /readyz, /metrics).
//
// It is the only package that imports the Prometheus client. Other packages
// declare the small Metrics interfaces they need and this package satisfies
// them. Nothing here logs or labels header values, bodies or query strings.
package observability

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HeaderRequestID is the header carrying the request identifier.
const HeaderRequestID = "X-Request-ID"

// NewLogger returns a slog.Logger writing to stderr at level, as JSON when
// json is true and as text otherwise.
func NewLogger(level slog.Level, json bool) *slog.Logger {
	// TODO(wave-2, observability agent): implement.
	return slog.Default()
}

// RequestID ensures every request carries an X-Request-ID, generating one when
// the client did not send one, and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	// TODO(wave-2, observability agent): implement.
	return next
}

// Metrics holds every Prometheus collector sluice exposes, together with the
// registry they are registered on. It satisfies proxy.Metrics and
// controller.Metrics. Labels are route keys and status codes only.
type Metrics struct {
	reg *prometheus.Registry
	// TODO(wave-2, observability agent): collectors.
}

// NewMetrics creates a private registry, registers the sluice collectors plus
// the standard Go and process collectors on it, and returns the handle.
// Keeping the registry here means no other package imports Prometheus.
func NewMetrics() *Metrics {
	// TODO(wave-2, observability agent): implement collectors.
	return &Metrics{reg: prometheus.NewRegistry()}
}

// Gatherer exposes the registry for tests and for AdminMux.
func (m *Metrics) Gatherer() prometheus.Gatherer { return m.reg }

// ObserveRequest records one completed request (proxy.Metrics).
func (m *Metrics) ObserveRequest(route string, status int, d time.Duration) {}

// UpstreamError records a failed upstream exchange (proxy.Metrics).
func (m *Metrics) UpstreamError(route, kind string) {}

// ConfigApplied records a published configuration (controller.Metrics).
func (m *Metrics) ConfigApplied(version uint64, routes int) {}

// ConfigRejected counts a snapshot that failed validation (controller.Metrics).
func (m *Metrics) ConfigRejected() {}

// IngressSkipped counts an Ingress skipped during translation (controller.Metrics).
func (m *Metrics) IngressSkipped() {}

// SetReady mirrors the readiness state into the sluice_ready gauge.
func (m *Metrics) SetReady(ready bool) {}

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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/readyz", r.Handler())
	mux.Handle("/metrics", promhttp.HandlerFor(m.Gatherer(), promhttp.HandlerOpts{}))
	return mux
}
