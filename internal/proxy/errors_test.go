package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
)

// blockingBackend signals when a request arrives, then holds it until the
// proxy gives up on the exchange (its context is cancelled) or the test
// timeout passes.
func blockingBackend(t *testing.T) (addr string, entered chan struct{}) {
	t.Helper()
	entered = make(chan struct{}, 1)
	addr = newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		signal(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(testTimeout):
		}
	})
	return addr, entered
}

// A backend that never sends headers within ResponseHeaderTimeout gives 504
// and an upstream error of kind timeout.
func TestResponseHeaderTimeoutIs504(t *testing.T) {
	a, _ := blockingBackend(t)
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m, ResponseHeaderTimeout: 50 * time.Millisecond}))

	start := time.Now()
	resp, _ := get(t, front, "app.example.com", "/", nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
	if d := time.Since(start); d > testTimeout/2 {
		t.Errorf("request took %v, the header timeout did not fire", d)
	}
	wantObservation(t, m.lastObserved(t), "default/app#r0p0", http.StatusGatewayTimeout)
	errs := m.upstreamErrors()
	if len(errs) != 1 || errs[0] != (upstreamError{route: "default/app#r0p0", kind: "timeout"}) {
		t.Errorf("UpstreamError calls = %v, want one (default/app#r0p0, timeout)", errs)
	}
}

// countingWriter counts WriteHeader and Write calls so a test can prove that
// nothing at all was written.
type countingWriter struct {
	http.ResponseWriter
	headers, writes int
}

func (c *countingWriter) WriteHeader(code int) {
	c.headers++
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.writes++
	return c.ResponseWriter.Write(b)
}

// A client cancelling before the upstream answered: nothing is written, the
// request is recorded as 499 and no upstream error is counted.
func TestClientCancelWritesNothingRecords499(t *testing.T) {
	a, entered := blockingBackend(t)
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	h := newHandler(t, store, Options{Metrics: m})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()
	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil).WithContext(ctx)
	cw := &countingWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(cw, req)

	if cw.headers != 0 || cw.writes != 0 {
		t.Errorf("WriteHeader called %d times and Write %d times, want 0 and 0", cw.headers, cw.writes)
	}
	wantObservation(t, m.lastObserved(t), "default/app#r0p0", StatusClientClosedRequest)
	if errs := m.upstreamErrors(); len(errs) != 0 {
		t.Errorf("UpstreamError calls = %v, want none for a client cancel", errs)
	}
}

// The same through a real server: the client closes its connection while the
// backend is busy, net/http cancels the request context, and the exchange is
// recorded as 499.
func TestClientCancelThroughServerRecords499(t *testing.T) {
	a, entered := blockingBackend(t)
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "app.example.com"
	errCh := make(chan error, 1)
	go func() {
		resp, err := front.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	wait(t, entered, "backend request")
	cancel()
	if err := wait(t, errCh, "client result"); !errors.Is(err, context.Canceled) {
		t.Fatalf("client error = %v, want context.Canceled", err)
	}

	wantObservation(t, m.lastObserved(t), "default/app#r0p0", StatusClientClosedRequest)
	if errs := m.upstreamErrors(); len(errs) != 0 {
		t.Errorf("UpstreamError calls = %v, want none for a client cancel", errs)
	}
}

// timeoutErr mimics the transport's unexported header-timeout error: a
// net.Error whose Timeout() is true that also satisfies
// errors.Is(err, context.DeadlineExceeded).
type timeoutErr struct{}

func (timeoutErr) Error() string        { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool        { return true }
func (timeoutErr) Temporary() bool      { return true }
func (timeoutErr) Is(target error) bool { return target == context.DeadlineExceeded }

func TestClassifyUpstreamError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
	readReset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

	cases := []struct {
		name   string
		err    error
		status int
		kind   string
	}{
		{"connection refused", refused, http.StatusBadGateway, "dial"},
		{"wrapped connection refused", fmt.Errorf("round trip: %w", refused), http.StatusBadGateway, "dial"},
		{"response header timeout", timeoutErr{}, http.StatusGatewayTimeout, "timeout"},
		{"dial timeout is a timeout", dialTimeout, http.StatusGatewayTimeout, "timeout"},
		{"inbound deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, "timeout"},
		{"read reset", readReset, http.StatusBadGateway, "other"},
		{"unexpected EOF", errors.New("unexpected EOF"), http.StatusBadGateway, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, kind := classify(tc.err)
			if status != tc.status || kind != tc.kind {
				t.Errorf("classify = (%d, %q), want (%d, %q)", status, kind, tc.status, tc.kind)
			}
		})
	}
}
