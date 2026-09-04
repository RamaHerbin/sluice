package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/router"
)

func TestNewAppliesDefaults(t *testing.T) {
	h := New(router.NewStore(), Options{})

	if h.transport == nil || h.rp == nil {
		t.Fatal("New did not create the transport and the reverse proxy")
	}
	if h.rp.Transport != h.transport {
		t.Error("ReverseProxy does not use the handler's transport")
	}
	if h.rp.Rewrite == nil {
		t.Error("ReverseProxy must use the Rewrite hook")
	}
	if h.rp.ModifyResponse == nil || h.rp.ErrorHandler == nil || h.rp.ErrorLog == nil {
		t.Error("ReverseProxy hooks are not all set")
	}

	want := Options{
		DialTimeout:           5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   64,
	}
	got := h.opts
	if got.DialTimeout != want.DialTimeout || got.ResponseHeaderTimeout != want.ResponseHeaderTimeout ||
		got.IdleConnTimeout != want.IdleConnTimeout || got.MaxIdleConnsPerHost != want.MaxIdleConnsPerHost {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	if got.Logger == nil || got.Metrics == nil {
		t.Error("Logger and Metrics must default to non-nil values")
	}
	if _, ok := got.Metrics.(NopMetrics); !ok {
		t.Errorf("Metrics default = %T, want NopMetrics", got.Metrics)
	}

	tr := h.transport
	if tr.Proxy != nil {
		t.Error("Transport.Proxy must stay nil")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("Transport.ForceAttemptHTTP2 must be false")
	}
	if tr.DialContext == nil {
		t.Error("Transport.DialContext is nil")
	}
	if tr.ResponseHeaderTimeout != 30*time.Second || tr.IdleConnTimeout != 90*time.Second || tr.MaxIdleConnsPerHost != 64 {
		t.Errorf("transport timeouts = (%v, %v, %d), want (30s, 90s, 64)",
			tr.ResponseHeaderTimeout, tr.IdleConnTimeout, tr.MaxIdleConnsPerHost)
	}
}

func TestNewKeepsExplicitOptions(t *testing.T) {
	m := &fakeMetrics{}
	h := New(router.NewStore(), Options{
		DialTimeout:           time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
		IdleConnTimeout:       3 * time.Second,
		MaxIdleConnsPerHost:   7,
		Logger:                discardLogger,
		Metrics:               m,
	})
	if h.opts.DialTimeout != time.Second || h.transport.ResponseHeaderTimeout != 2*time.Second ||
		h.transport.IdleConnTimeout != 3*time.Second || h.transport.MaxIdleConnsPerHost != 7 {
		t.Errorf("explicit options were replaced: %+v", h.opts)
	}
	if h.opts.Logger != discardLogger || h.opts.Metrics != Metrics(m) {
		t.Error("explicit Logger or Metrics were replaced")
	}
}

// Scenario 1 at the proxy level: the request reaches the service selected by
// its hostname, with the same host normalization router.Match applies.
func TestHostRoutingThroughHandler(t *testing.T) {
	a, _ := textBackend(t, "A")
	b, _ := textBackend(t, "B")
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a), backend(beB, b)},
		route("app-a", "a.example.com", "/", beA),
		route("app-b", "b.example.com", "/", beB),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m}))

	cases := []struct {
		host   string
		status int
		body   string
		route  string
	}{
		{"a.example.com", http.StatusOK, "A", "default/app-a#r0p0"},
		{"b.example.com", http.StatusOK, "B", "default/app-b#r0p0"},
		{"A.Example.COM:8080", http.StatusOK, "A", "default/app-a#r0p0"},
		{"unknown.example.com", http.StatusNotFound, "", ""},
	}
	for i, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			resp, body := get(t, front, tc.host, "/", nil)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.body != "" && body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
			obs := m.waitObserved(t, i+1)
			wantObservation(t, obs[i], tc.route, tc.status)
		})
	}
}

func TestUnknownHostIs404(t *testing.T) {
	a, hits := textBackend(t, "A")
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	h := newHandler(t, store, Options{Metrics: m})

	rec := serve(h, "http://other.example.com/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "404") {
		t.Errorf("body = %q, want a plain-text 404 message", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if hits.Load() != 0 {
		t.Errorf("backend received %d requests, want 0", hits.Load())
	}
	wantObservation(t, m.lastObserved(t), "", http.StatusNotFound)
	if errs := m.upstreamErrors(); len(errs) != 0 {
		t.Errorf("UpstreamError calls = %v, want none for a 404", errs)
	}
}

// Scenario 3: two endpoints receive requests in round-robin.
func TestScenario03RoundRobinAlternates(t *testing.T) {
	a, hitsA := textBackend(t, "A")
	b, hitsB := textBackend(t, "B")
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a, b)},
		route("app", "app.example.com", "/", beA),
	))
	front := newFront(t, newHandler(t, store, Options{}))

	var got []string
	for range 4 {
		_, body := get(t, front, "app.example.com", "/", nil)
		got = append(got, body)
	}
	// Compile sorts endpoint addresses, so the first pick is the smaller
	// address; the test only cares that consecutive picks alternate.
	if got[0] == got[1] || got[0] != got[2] || got[1] != got[3] {
		t.Errorf("bodies = %v, want strict alternation between A and B", got)
	}
	if hitsA.Load() != 2 || hitsB.Load() != 2 {
		t.Errorf("hits A=%d B=%d, want 2 and 2", hitsA.Load(), hitsB.Load())
	}
}

// Scenario 4 (proxy side): after a swap that removed an endpoint, it is no
// longer used.
func TestScenario04RemovedEndpointNoLongerUsed(t *testing.T) {
	a, hitsA := textBackend(t, "A")
	b, hitsB := textBackend(t, "B")
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a, b)},
		route("app", "app.example.com", "/", beA),
	))
	front := newFront(t, newHandler(t, store, Options{}))

	for range 4 {
		get(t, front, "app.example.com", "/", nil)
	}
	if hitsA.Load() != 2 || hitsB.Load() != 2 {
		t.Fatalf("before swap: hits A=%d B=%d, want 2 and 2", hitsA.Load(), hitsB.Load())
	}

	mustApply(t, store, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	for range 4 {
		_, body := get(t, front, "app.example.com", "/", nil)
		if body != "A" {
			t.Errorf("after swap: body = %q, want A", body)
		}
	}
	if hitsA.Load() != 6 || hitsB.Load() != 2 {
		t.Errorf("after swap: hits A=%d B=%d, want 6 and 2", hitsA.Load(), hitsB.Load())
	}
}

// Scenario 8: a route with no ready endpoint answers 503.
func TestScenario08NoReadyEndpointIs503(t *testing.T) {
	a, hits := textBackend(t, "A")
	notReady := model.Backend{Key: beA, Endpoints: []model.Endpoint{{Address: a, Ready: false}}}
	store := newStore(t, snapshot(
		[]model.Backend{notReady, backend(beB)},
		route("app", "app.example.com", "/", beA),
		route("empty", "empty.example.com", "/", beB),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m}))

	for _, host := range []string{"app.example.com", "empty.example.com"} {
		resp, body := get(t, front, host, "/", nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", host, resp.StatusCode)
		}
		if !strings.Contains(body, "503") {
			t.Errorf("%s: body = %q, want a plain-text 503 message", host, body)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("not-ready backend received %d requests, want 0", hits.Load())
	}
	obs := m.waitObserved(t, 2)
	wantObservation(t, obs[0], "default/app#r0p0", http.StatusServiceUnavailable)
	wantObservation(t, obs[1], "default/empty#r0p0", http.StatusServiceUnavailable)
	if errs := m.upstreamErrors(); len(errs) != 0 {
		t.Errorf("UpstreamError calls = %v, want none: no exchange was attempted", errs)
	}
}

// Scenario 9: an unreachable upstream answers 502 and counts a dial error.
func TestScenario09UnreachableUpstreamIs502(t *testing.T) {
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, closedPort(t))},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m}))

	resp, body := get(t, front, "app.example.com", "/", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(body, "502") {
		t.Errorf("body = %q, want a plain-text 502 message", body)
	}
	wantObservation(t, m.lastObserved(t), "default/app#r0p0", http.StatusBadGateway)
	errs := m.upstreamErrors()
	if len(errs) != 1 || errs[0] != (upstreamError{route: "default/app#r0p0", kind: "dial"}) {
		t.Errorf("UpstreamError calls = %v, want one (default/app#r0p0, dial)", errs)
	}
}

func TestCloseIdleConnections(t *testing.T) {
	a, _ := textBackend(t, "A")
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	h := New(store, Options{Logger: discardLogger})

	if rec := serve(h, "http://app.example.com/", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The exchange left an idle connection in the pool. Closing must not
	// break later requests: the transport simply dials again.
	h.CloseIdleConnections()
	if rec := serve(h, "http://app.example.com/", nil); rec.Code != http.StatusOK {
		t.Errorf("status after CloseIdleConnections = %d, want 200", rec.Code)
	}
	h.CloseIdleConnections()
}

// The access log carries the documented fields and nothing sensitive: the
// query string never appears.
func TestAccessLogFields(t *testing.T) {
	a, _ := textBackend(t, "A")
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := newHandler(t, store, Options{Logger: logger})

	header := http.Header{"X-Request-Id": {"rid-1"}, "Authorization": {"Bearer secret-token"}}
	rec := serve(h, "http://app.example.com/foo%20bar?token=secret-query", header)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var line map[string]any
	found := false
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		if rec["msg"] == "request" {
			line, found = rec, true
		}
	}
	if !found {
		t.Fatalf("no access log line in:\n%s", buf.String())
	}
	want := map[string]any{
		"level":      "INFO",
		"request_id": "rid-1",
		"method":     "GET",
		"host":       "app.example.com",
		"path":       "/foo%20bar",
		"status":     float64(200),
		"route":      "default/app#r0p0",
		"upstream":   a,
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log field %s = %v, want %v", k, line[k], v)
		}
	}
	if d, ok := line["duration"].(float64); !ok || d <= 0 {
		t.Errorf("log field duration = %v, want a positive number", line["duration"])
	}
	for _, leak := range []string{"secret", "Bearer", "token"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("log output leaks %q:\n%s", leak, buf.String())
		}
	}
}

// The access log names the route "none" when nothing matched.
func TestAccessLogUnmatchedRouteIsNone(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := newHandler(t, router.NewStore(), Options{Logger: logger})

	rec := serve(h, "http://nowhere.example.com/x", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", buf.String(), err)
	}
	if line["route"] != "none" || line["status"] != float64(404) || line["upstream"] != "" {
		t.Errorf("log line = %v, want route none, status 404, empty upstream", line)
	}
}
