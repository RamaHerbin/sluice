package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/router"
)

// testTimeout bounds every wait in this package so a broken proxy fails a
// test instead of hanging it.
const testTimeout = 5 * time.Second

// Backend keys shared by the tests.
var (
	beA = model.BackendKey{Namespace: "default", Service: "svc-a", Port: 80}
	beB = model.BackendKey{Namespace: "default", Service: "svc-b", Port: 80}
)

// discardLogger keeps the test output quiet. Tests that assert on log lines
// build their own logger.
var discardLogger = slog.New(slog.DiscardHandler)

// backend returns a Backend whose endpoints are all ready.
func backend(key model.BackendKey, addrs ...string) model.Backend {
	b := model.Backend{Key: key}
	for _, a := range addrs {
		b.Endpoints = append(b.Endpoints, model.Endpoint{Address: a, Ready: true})
	}
	return b
}

// route builds a Prefix route keyed "default/<name>#r0p0".
func route(name, host, path string, key model.BackendKey) model.Route {
	return model.Route{
		Key:      model.RouteKey{Namespace: "default", Name: name},
		Host:     host,
		Path:     path,
		PathType: model.PathPrefix,
		Backend:  key,
	}
}

// snapshot assembles routes and backends.
func snapshot(backends []model.Backend, routes ...model.Route) *model.Snapshot {
	return &model.Snapshot{Routes: routes, Backends: backends}
}

// mustApply publishes snap on s or fails the test.
func mustApply(t *testing.T, s *router.Store, snap *model.Snapshot) *router.Table {
	t.Helper()
	tbl, _, err := s.Apply(snap)
	if err != nil {
		t.Fatalf("Store.Apply: %v", err)
	}
	return tbl
}

// newStore returns a Store already publishing snap.
func newStore(t *testing.T, snap *model.Snapshot) *router.Store {
	t.Helper()
	s := router.NewStore()
	mustApply(t, s, snap)
	return s
}

// newHandler builds a Handler with a quiet logger unless o.Logger is set, and
// closes its idle connections when the test ends.
func newHandler(t *testing.T, p Provider, o Options) *Handler {
	t.Helper()
	if o.Logger == nil {
		o.Logger = discardLogger
	}
	h := New(p, o)
	t.Cleanup(h.CloseIdleConnections)
	return h
}

// newBackend starts an httptest.Server running fn and returns its host:port.
func newBackend(t *testing.T, fn http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	return addrOf(srv)
}

// addrOf returns the host:port of an httptest.Server.
func addrOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

// textBackend answers body with status 200 and counts its hits.
func textBackend(t *testing.T, body string) (addr string, hits *atomic.Int64) {
	t.Helper()
	hits = new(atomic.Int64)
	addr = newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		respond(w, body)
	})
	return addr, hits
}

// respond writes body. The error is ignored on purpose: the client's view of
// the exchange is what the tests assert.
func respond(w http.ResponseWriter, body string) {
	_, _ = io.WriteString(w, body)
}

// closedPort returns a loopback address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// newFront starts a real HTTP server in front of h, so tests observe the
// behavior of net/http (connection close, context cancellation, panics)
// rather than that of a recorder.
func newFront(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return front
}

// get sends GET path to front with the given Host header and extra headers,
// and returns the response with its body already read.
func get(t *testing.T, front *httptest.Server, host, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, front.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	for k, vs := range header {
		req.Header[k] = slices.Clone(vs)
	}
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", host, path, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

// serve runs one request through h with a recorder. target is an absolute
// URL whose host becomes the Host header.
func serve(h http.Handler, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, vs := range header {
		req.Header[k] = slices.Clone(vs)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// signal performs a non-blocking send on a buffered channel, so a backend
// handler can report "request received" without blocking a second caller.
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// wait receives from ch or fails the test after testTimeout.
func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// observation is one ObserveRequest call.
type observation struct {
	route  string
	status int
	d      time.Duration
}

// upstreamError is one UpstreamError call.
type upstreamError struct {
	route, kind string
}

// fakeMetrics records every call so tests can assert on labels and status.
type fakeMetrics struct {
	mu       sync.Mutex
	requests []observation
	errors   []upstreamError
}

func (m *fakeMetrics) ObserveRequest(route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, observation{route: route, status: status, d: d})
}

func (m *fakeMetrics) UpstreamError(route, kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, upstreamError{route: route, kind: kind})
}

// observed returns a copy of the recorded requests.
func (m *fakeMetrics) observed() []observation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// upstreamErrors returns a copy of the recorded upstream errors.
func (m *fakeMetrics) upstreamErrors() []upstreamError {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.errors)
}

// waitObserved polls until at least n requests were recorded. Behind a real
// server the deferred metrics call may run after the client already has its
// response, so tests cannot read the slice right away.
func (m *fakeMetrics) waitObserved(t *testing.T, n int) []observation {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		got := m.observed()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d requests, want at least %d", len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lastObserved returns the most recent observation or fails the test.
func (m *fakeMetrics) lastObserved(t *testing.T) observation {
	t.Helper()
	got := m.waitObserved(t, 1)
	return got[len(got)-1]
}

// wantObservation asserts route and status of an observation and that its
// duration is positive.
func wantObservation(t *testing.T, got observation, route string, status int) {
	t.Helper()
	if got.route != route || got.status != status {
		t.Errorf("ObserveRequest(route=%q, status=%d), want (%q, %d)", got.route, got.status, route, status)
	}
	if got.d <= 0 {
		t.Errorf("ObserveRequest duration = %v, want > 0", got.d)
	}
}
