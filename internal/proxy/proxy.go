// Package proxy is the data plane: an http.Handler that routes each request
// with the current routing table and forwards it with httputil.ReverseProxy.
//
// The handler loads the table pointer exactly once per request, so a
// configuration swap never affects a request already in flight. It never reads
// Kubernetes objects.
package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/RamaHerbin/sluice/internal/router"
)

// Provider yields the current routing table. *router.Store satisfies it.
type Provider interface {
	Current() *router.Table
}

// Metrics is the consumer-side metrics surface the proxy needs.
type Metrics interface {
	// ObserveRequest records one completed request. route is the route key
	// or "" when no route matched.
	ObserveRequest(route string, status int, d time.Duration)
	// UpstreamError records a failed upstream exchange. kind is one of
	// "dial", "timeout", "other".
	UpstreamError(route, kind string)
}

// NopMetrics discards every observation.
type NopMetrics struct{}

func (NopMetrics) ObserveRequest(string, int, time.Duration) {}
func (NopMetrics) UpstreamError(string, string)              {}

// Options configures a Handler. Zero values take the defaults documented on
// each field.
type Options struct {
	DialTimeout           time.Duration // default 5s
	ResponseHeaderTimeout time.Duration // default 30s
	IdleConnTimeout       time.Duration // default 90s
	MaxIdleConnsPerHost   int           // default 64
	Logger                *slog.Logger  // default slog.Default()
	Metrics               Metrics       // default NopMetrics{}
}

// Handler is the reverse proxy. Create it with New.
type Handler struct {
	provider Provider
	opts     Options
	// TODO(wave-2, proxy+upstream agent): one *http.Transport and one
	// *httputil.ReverseProxy shared for the process lifetime.
}

// New builds a Handler. It creates exactly one http.Transport and one
// httputil.ReverseProxy, shared by all requests.
func New(p Provider, o Options) *Handler {
	// TODO(wave-2, proxy+upstream agent): implement defaults and wiring.
	return &Handler{provider: p, opts: o}
}

// ServeHTTP matches the request against the current table (404 when nothing
// matches), picks an endpoint (503 when none is ready) and proxies it. Upstream
// failures produce 502, upstream timeouts 504.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO(wave-2, proxy+upstream agent): implement.
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// CloseIdleConnections closes idle upstream connections. Call it after the
// HTTP server has finished shutting down.
func (h *Handler) CloseIdleConnections() {
	// TODO(wave-2, proxy+upstream agent): implement.
}
