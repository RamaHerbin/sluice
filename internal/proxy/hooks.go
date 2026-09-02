package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/RamaHerbin/sluice/internal/placeholders"
	"github.com/RamaHerbin/sluice/internal/router"
)

// stateKey is the context key under which ServeHTTP stores the requestState.
// An unexported struct type cannot collide with keys from other packages.
type stateKey struct{}

// requestState is the per-request data ServeHTTP hands to the ReverseProxy
// hooks through the request context. ReverseProxy clones the request with a
// context derived from the inbound one, so the same pointer is visible from
// Rewrite (pr.In and pr.Out), ModifyResponse (resp.Request) and ErrorHandler.
//
// The hooks run on the handler goroutine, one after the other, so the fields
// need no synchronization.
type requestState struct {
	// in is the inbound request as the proxy received it. Placeholders must
	// read it, never the rewritten outbound clone, so that request and
	// response rules see the same values and SetXForwarded or earlier rules
	// never feed back into templates.
	in    *http.Request
	route *router.Route
	// addr is the host:port returned by the picker.
	addr string
	// clientClosed is set by errorHandler when the client cancelled before
	// any response header was written; finish then records
	// StatusClientClosedRequest instead of the captured status.
	clientClosed bool
}

// stateFrom returns the requestState stored in ctx, or nil.
func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(stateKey{}).(*requestState)
	return st
}

// placeholderContext builds the evaluation context shared by request and
// response header rules.
func (st *requestState) placeholderContext() placeholders.Context {
	return placeholders.Context{
		Request:         st.in,
		Namespace:       st.route.Namespace,
		Service:         st.route.Service,
		Route:           st.route.Key.Namespace + "/" + st.route.Key.Name,
		UpstreamAddress: st.addr,
	}
}

// rewrite is the ReverseProxy.Rewrite hook. It points the outbound request at
// the picked endpoint, keeps the original Host (Ingress semantics), sets the
// X-Forwarded-* headers from our view of the connection and applies the
// request header rules.
func (h *Handler) rewrite(pr *httputil.ProxyRequest) {
	st := stateFrom(pr.In.Context())
	if st == nil {
		// Unreachable through ServeHTTP. Left untouched, the outbound URL
		// has no scheme, the transport fails the exchange and errorHandler
		// answers 502 rather than this hook guessing a target.
		return
	}
	pr.SetURL(&url.URL{Scheme: "http", Host: st.addr})
	// SetURL blanks Out.Host, which would make the transport send the
	// upstream address as Host. The backend must see the client's Host.
	pr.Out.Host = pr.In.Host
	// ReverseProxy already deleted Forwarded and X-Forwarded-* from the
	// outbound clone, so this sets X-Forwarded-For to the peer IP only,
	// X-Forwarded-Host to the inbound Host and X-Forwarded-Proto to http or
	// https from In.TLS. Client-supplied values never survive (no
	// trusted-proxy list in the MVP).
	pr.SetXForwarded()
	applyHeaderRules(pr.Out.Header, st.route.RequestHeaders, st)
}

// modifyResponse is the ReverseProxy.ModifyResponse hook. It applies the
// response header rules before the status line is written to the client.
func (h *Handler) modifyResponse(resp *http.Response) error {
	if resp.Request == nil {
		return nil
	}
	// resp.Request is the rewritten outbound clone: it carries our
	// X-Forwarded-* headers and has lost hop-by-hop ones. Only its context
	// is used here; the templates read the inbound request from the state.
	st := stateFrom(resp.Request.Context())
	if st == nil {
		return nil
	}
	applyHeaderRules(resp.Header, st.route.ResponseHeaders, st)
	return nil
}

// applyHeaderRules evaluates rules in order and sets each non-empty result
// on dst. An empty result leaves the header untouched, which is how "copy
// this header if present" works.
func applyHeaderRules(dst http.Header, rules []router.Header, st *requestState) {
	if len(rules) == 0 {
		return
	}
	c := st.placeholderContext()
	for _, rule := range rules {
		if v := rule.Value.Evaluate(c); v != "" {
			dst.Set(rule.Name, v)
		}
	}
}

// errorHandler is the ReverseProxy.ErrorHandler hook. It is reached only for
// failures before the response headers were copied to the client; a client
// leaving mid-body surfaces as panic(http.ErrAbortHandler) instead.
//
// r is the outbound clone, which shares the inbound context and therefore
// the requestState.
func (h *Handler) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	st := stateFrom(r.Context())
	routeKey, addr := "", ""
	if st != nil {
		routeKey = st.route.Key.String()
		addr = st.addr
	}

	// Checked first: a cancelled dial is also a *net.OpError with Op "dial",
	// and must not count as an upstream failure.
	if errors.Is(err, context.Canceled) {
		// The client left before any response header was written. Nothing
		// can reach it, so write nothing and let finish record 499. The
		// server finalizes the dead connection on its own.
		if st != nil {
			st.clientClosed = true
		}
		h.opts.Logger.Debug("client closed request", "route", routeKey, "upstream", addr)
		return
	}

	status, kind := classify(err)
	h.opts.Metrics.UpstreamError(routeKey, kind)
	// err names the upstream address and the failure, never header values or
	// bodies.
	h.opts.Logger.Warn("upstream error",
		"route", routeKey, "upstream", addr, "kind", kind, "status", status, "err", err)
	writeStatus(w, status, "")
}

// classify maps an upstream error to the status sent to the client and the
// kind label of the upstream-error metric. The caller has already excluded
// context.Canceled.
//
// Timeout is checked before dial: the transport's ResponseHeaderTimeout error
// is an unexported type that only exposes net.Error, and a dial that timed
// out is an *net.OpError whose Timeout() is true, reported as a timeout as
// well. errors.Is(err, context.DeadlineExceeded) is deliberately not used:
// on Go 1.26 the header-timeout error satisfies it too, so it cannot tell an
// inbound deadline from an upstream one.
func classify(err error) (status int, kind string) {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout, "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return http.StatusBadGateway, "dial"
	}
	return http.StatusBadGateway, "other"
}
