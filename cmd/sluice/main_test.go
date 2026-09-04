package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/observability"
	"github.com/RamaHerbin/sluice/internal/proxy"
	"github.com/RamaHerbin/sluice/internal/router"
)

// TestStaticRouteEndToEnd wires the same components main does, minus the
// Kubernetes controller, around one hand-built snapshot: the slice 1
// integration test kept after the static -route flag was removed (ADR 0006).
func TestStaticRouteEndToEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from backend, host="+r.Host)
	}))
	defer backend.Close()

	key := model.BackendKey{Namespace: "demo", Service: "echo", Port: 80}
	snap := &model.Snapshot{
		Routes: []model.Route{{
			Key:      model.RouteKey{Namespace: "demo", Name: "demo"},
			Host:     "demo.local",
			Path:     "/",
			PathType: model.PathPrefix,
			Backend:  key,
			Created:  time.Now(),
		}},
		Backends: []model.Backend{{
			Key:       key,
			Endpoints: []model.Endpoint{{Address: strings.TrimPrefix(backend.URL, "http://"), Ready: true}},
		}},
	}
	store := router.NewStore()
	table, changed, err := store.Apply(snap)
	if err != nil || !changed || table.Version != 1 || table.Len() != 1 {
		t.Fatalf("Apply: table=%+v changed=%v err=%v", table, changed, err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := proxy.New(store, proxy.Options{Logger: quiet})
	defer handler.CloseIdleConnections()
	front := httptest.NewServer(observability.RequestID(handler))
	defer front.Close()

	get := func(host string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, front.URL+"/anything", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := get("demo.local")
	if resp.StatusCode != http.StatusOK || body != "hello from backend, host=demo.local" {
		t.Fatalf("matched route: status=%d body=%q", resp.StatusCode, body)
	}
	if resp.Header.Get(observability.HeaderRequestID) == "" {
		t.Error("response has no X-Request-ID")
	}
	resp, _ = get("other.local")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown host: status=%d, want 404", resp.StatusCode)
	}
}
