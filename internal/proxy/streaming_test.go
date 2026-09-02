package proxy

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
)

// Chunks flushed by the backend reach the client one by one. The backend
// only sends the next chunk once the test has read the previous one, so a
// proxy that buffered the body would deadlock and hit the client timeout.
func TestStreamingChunksArriveProgressively(t *testing.T) {
	const chunks = 3
	proceed := make(chan struct{})
	a := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		for i := range chunks {
			if i > 0 {
				select {
				case <-proceed:
				case <-r.Context().Done():
					return
				}
			}
			if _, err := fmt.Fprintf(w, "chunk %d\n", i); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	})
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	front := newFront(t, newHandler(t, store, Options{}))

	client := front.Client()
	client.Timeout = testTimeout
	req, err := http.NewRequest(http.MethodGet, front.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "app.example.com"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	for i := range chunks {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("chunk %d: %v (the proxy did not stream)", i, err)
		}
		if want := fmt.Sprintf("chunk %d\n", i); line != want {
			t.Fatalf("chunk %d = %q, want %q", i, line, want)
		}
		if i < chunks-1 {
			proceed <- struct{}{}
		}
	}
}

// ReverseProxy forwards 103 Early Hints before the final response. The
// recorded status must be the final one.
func TestEarlyHintsRecordFinalStatus(t *testing.T) {
	a := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusOK)
		respond(w, "ok")
	})
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m}))

	resp, body := get(t, front, "app.example.com", "/", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("status %d body %q, want 200 ok", resp.StatusCode, body)
	}
	wantObservation(t, m.lastObserved(t), "default/app#r0p0", http.StatusOK)
}

// A client leaving mid-body never reaches ErrorHandler: ReverseProxy panics
// with http.ErrAbortHandler, net/http recovers it, and the deferred log and
// metrics still record the 200 that was already sent.
func TestClientLeavesMidBodyRecordsStatus200(t *testing.T) {
	backendDone := make(chan struct{}, 1)
	a := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		defer signal(backendDone)
		rc := http.NewResponseController(w)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		// Keep streaming until the proxy drops the upstream connection,
		// which is how the abort shows up on this side.
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintln(w, strings.Repeat("x", 4096)); err != nil {
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}
			}
		}
	})
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	m := &fakeMetrics{}
	front := newFront(t, newHandler(t, store, Options{Metrics: m}))

	req, err := http.NewRequest(http.MethodGet, front.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "app.example.com"
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	// Closing a partially read body makes the transport close the
	// connection, so the proxy's next write to the client fails.
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	wantObservation(t, m.lastObserved(t), "default/app#r0p0", http.StatusOK)
	if errs := m.upstreamErrors(); len(errs) != 0 {
		t.Errorf("UpstreamError calls = %v, want none: the upstream did nothing wrong", errs)
	}
	// The unwind closed the upstream body, which ends the backend handler.
	wait(t, backendDone, "backend handler to return")

	// The front server survived the panic and still serves.
	b, _ := textBackend(t, "B")
	mustApply(t, store, snapshot([]model.Backend{backend(beA, b)}, route("app", "app.example.com", "/", beA)))
	if resp, body := get(t, front, "app.example.com", "/", nil); resp.StatusCode != http.StatusOK || body != "B" {
		t.Errorf("after abort: status %d body %q, want 200 B", resp.StatusCode, body)
	}
}

func TestStatusWriter(t *testing.T) {
	cases := []struct {
		name string
		run  func(w http.ResponseWriter)
		want int
	}{
		{"nothing written", func(http.ResponseWriter) {}, 0},
		{"informational then final", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusOK)
		}, http.StatusOK},
		{"100 continue ignored", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusContinue)
			w.WriteHeader(http.StatusCreated)
		}, http.StatusCreated},
		{"switching protocols is final", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusSwitchingProtocols)
		}, http.StatusSwitchingProtocols},
		{"implicit 200 on write", func(w http.ResponseWriter) {
			respond(w, "body")
		}, http.StatusOK},
		{"error status", func(w http.ResponseWriter) {
			http.Error(w, "bad", http.StatusBadGateway)
		}, http.StatusBadGateway},
		{"first final wins", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNoContent)
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw := &statusWriter{ResponseWriter: httptest.NewRecorder()}
			tc.run(sw)
			if sw.status != tc.want {
				t.Errorf("status = %d, want %d", sw.status, tc.want)
			}
		})
	}
}

// Unwrap lets http.ResponseController reach the Flusher underneath, which
// ReverseProxy relies on to stream.
func TestStatusWriterUnwrapReachesFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	if sw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap did not return the wrapped writer")
	}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("Flush through ResponseController: %v", err)
	}
	if !rec.Flushed {
		t.Error("the underlying recorder was not flushed")
	}
}
