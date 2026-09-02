// Package proxy is the data plane: an http.Handler that routes each request
// with the current routing table and forwards it with httputil.ReverseProxy.
//
// The handler loads the table pointer exactly once per request, so a
// configuration swap never affects a request already in flight. It never reads
// Kubernetes objects.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/RamaHerbin/sluice/internal/router"
)

// Provider yields the current routing table. *router.Store satisfies it.
type Provider interface {
	Current() *router.Table
}

// StatusClientClosedRequest is the synthetic status recorded in metrics and
// access logs when the client went away before any response header was
// written (nginx convention). It is never sent on the wire.
const StatusClientClosedRequest = 499

// Metrics is the consumer-side metrics surface the proxy needs.
type Metrics interface {
	// ObserveRequest records one completed request. route is the route key
	// or "" when no route matched. status is the code sent to the client, or
	// StatusClientClosedRequest when the client cancelled before headers.
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

// Default values applied by New to zero Options fields.
const (
	defaultDialTimeout           = 5 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultIdleConnTimeout       = 90 * time.Second
	defaultMaxIdleConnsPerHost   = 64
	// dialKeepAlive is the TCP keep-alive probe interval on upstream
	// connections. It is not configurable: it only matters for detecting
	// dead peers behind idle pooled connections.
	dialKeepAlive = 30 * time.Second
)

// Handler is the reverse proxy. Create it with New.
type Handler struct {
	provider Provider
	// opts holds the options with defaults applied.
	opts Options
	// transport and rp are created once in New and shared by every request
	// for the process lifetime. Per-request data never lives on them; it
	// travels through the request context (see requestState).
	transport *http.Transport
	rp        *httputil.ReverseProxy
}

// New builds a Handler. It creates exactly one http.Transport and one
// httputil.ReverseProxy, shared by all requests.
func New(p Provider, o Options) *Handler {
	if o.DialTimeout <= 0 {
		o.DialTimeout = defaultDialTimeout
	}
	if o.ResponseHeaderTimeout <= 0 {
		o.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	}
	if o.IdleConnTimeout <= 0 {
		o.IdleConnTimeout = defaultIdleConnTimeout
	}
	if o.MaxIdleConnsPerHost <= 0 {
		o.MaxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = NopMetrics{}
	}

	dialer := &net.Dialer{Timeout: o.DialTimeout, KeepAlive: dialKeepAlive}
	transport := &http.Transport{
		// Proxy stays nil: upstreams are cluster-local pod addresses and a
		// fresh Transport must not pick up HTTP_PROXY from the environment
		// the way http.DefaultTransport does.
		Proxy:       nil,
		DialContext: dialer.DialContext,
		// Backends are spoken to in plain HTTP/1.1. Nothing in the MVP
		// negotiates HTTP/2 with pods.
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   o.MaxIdleConnsPerHost,
		IdleConnTimeout:       o.IdleConnTimeout,
		ResponseHeaderTimeout: o.ResponseHeaderTimeout,
	}

	h := &Handler{provider: p, opts: o, transport: transport}
	h.rp = &httputil.ReverseProxy{
		Rewrite:        h.rewrite,
		ModifyResponse: h.modifyResponse,
		ErrorHandler:   h.errorHandler,
		// ReverseProxy logs a few internal conditions (for example a read
		// error while copying a body) through ErrorLog. They are noise for
		// operators because errorHandler already logs what matters, so they
		// are bridged to the structured logger at debug level.
		ErrorLog:  slog.NewLogLogger(o.Logger.Handler(), slog.LevelDebug),
		Transport: transport,
	}
	return h
}

// ServeHTTP matches the request against the current table (404 when nothing
// matches), picks an endpoint (503 when none is ready) and proxies it. Upstream
// failures produce 502, upstream timeouts 504.
//
// Implementation notes validated by the wave 1 prototype
// (docs/design/dataplane.md):
//   - Load the table once, then carry route, upstream address and the
//     inbound *http.Request through the request context; resp.Request in
//     ModifyResponse is the rewritten outbound clone and must not feed
//     placeholders.Context.Request.
//   - Rewrite: SetURL, Out.Host = In.Host, SetXForwarded (client-supplied
//     X-Forwarded-* are dropped by ReverseProxy before the hook), then
//     request header templates.
//   - ErrorHandler classification order: context.Canceled (write nothing,
//     record StatusClientClosedRequest), then net.Error with Timeout()==true
//     (504, kind "timeout"), then *net.OpError with Op=="dial" (502, kind
//     "dial"), then anything else (502, kind "other"). Do not use
//     errors.Is(err, context.DeadlineExceeded): the transport's header
//     timeout error also satisfies it on Go 1.26.
//   - The status-capturing ResponseWriter ignores WriteHeader codes below 200
//     except 101, because ReverseProxy forwards 103 Early Hints.
//   - A client leaving mid-body never reaches ErrorHandler; it surfaces as
//     panic(http.ErrAbortHandler) under a real server. The deferred access
//     log and metrics must tolerate that unwind with the status already set.
//   - Transport.Proxy stays nil; there is one Transport and one ReverseProxy.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	st := &requestState{in: r}
	// No recover here, on purpose. When the client leaves mid-body,
	// ReverseProxy panics with http.ErrAbortHandler. The deferred call runs
	// during the unwind with the status already captured, then the panic
	// continues to net/http, which recovers that sentinel silently and
	// closes the connection. Recovering and re-panicking here would only
	// hide the original stack of any other panic.
	defer h.finish(sw, st, start)

	// One load per request. Everything needed afterwards is reachable from
	// this pointer, so a swap during the exchange is invisible to it.
	t := h.provider.Current()
	if t == nil {
		// router.Store never returns nil; another Provider might.
		writeStatus(sw, http.StatusNotFound, "no route matches this host and path")
		return
	}
	// Match runs on the decoded path (r.URL.Path), the form Ingress paths are
	// written in. The access log and {http.request.path} use EscapedPath()
	// so a percent-encoded CR LF can never reach a log line or a header.
	route, ok := t.Match(r.Host, r.URL.Path)
	if !ok {
		writeStatus(sw, http.StatusNotFound, "no route matches this host and path")
		return
	}
	st.route = route

	addr, ok := route.Backend.Pick()
	if !ok {
		writeStatus(sw, http.StatusServiceUnavailable, "no ready endpoint for this route")
		return
	}
	st.addr = addr

	ctx := context.WithValue(r.Context(), stateKey{}, st)
	h.rp.ServeHTTP(sw, r.WithContext(ctx))
}

// CloseIdleConnections closes idle upstream connections. Call it after the
// HTTP server has finished shutting down.
func (h *Handler) CloseIdleConnections() {
	h.transport.CloseIdleConnections()
}

// finish records the request in metrics and the access log. It runs deferred
// from ServeHTTP, including during an http.ErrAbortHandler unwind.
func (h *Handler) finish(sw *statusWriter, st *requestState, start time.Time) {
	d := time.Since(start)
	status := sw.status
	switch {
	case st.clientClosed:
		status = StatusClientClosedRequest
	case status == 0:
		// Nothing was written. net/http completes such a response with an
		// implicit 200, so that is what went on the wire.
		status = http.StatusOK
	}
	routeKey := ""
	if st.route != nil {
		routeKey = st.route.Key.String()
	}
	h.opts.Metrics.ObserveRequest(routeKey, status, d)
	h.logAccess(st, status, d, routeKey)
}

// logAccess writes one access log line at info level. It never logs header
// values other than the request id, nor bodies, nor the query string.
func (h *Handler) logAccess(st *requestState, status int, d time.Duration, routeKey string) {
	log := h.opts.Logger
	ctx := st.in.Context()
	if !log.Enabled(ctx, slog.LevelInfo) {
		return
	}
	if routeKey == "" {
		routeKey = "none"
	}
	log.LogAttrs(ctx, slog.LevelInfo, "request",
		// The request-ID middleware guarantees this header exists in the
		// real process; tests without it log an empty id.
		slog.String("request_id", st.in.Header.Get("X-Request-ID")),
		slog.String("method", st.in.Method),
		slog.String("host", st.in.Host),
		// The wire form of the path, without the query string. Unlike
		// URL.Path it cannot carry control bytes into the log.
		slog.String("path", st.in.URL.EscapedPath()),
		slog.Int("status", status),
		slog.Duration("duration", d),
		slog.String("route", routeKey),
		slog.String("upstream", st.addr),
	)
}

// writeStatus answers a plain-text error the way http.Error does. The body
// carries the numeric code so a curl user sees it without -i.
func writeStatus(w http.ResponseWriter, code int, detail string) {
	msg := fmt.Sprintf("%d %s", code, http.StatusText(code))
	if detail != "" {
		msg += ": " + detail
	}
	http.Error(w, msg, code)
}
