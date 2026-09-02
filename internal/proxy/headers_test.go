package proxy

import (
	"net/http"
	"testing"

	"github.com/RamaHerbin/sluice/internal/model"
)

// captured is what a backend saw on one request.
type captured struct {
	host   string
	header http.Header
}

// capturingBackend records the Host and headers of each request it receives
// and answers "ok".
func capturingBackend(t *testing.T) (addr string, seen chan captured) {
	t.Helper()
	seen = make(chan captured, 8)
	addr = newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- captured{host: r.Host, header: r.Header.Clone()}
		respond(w, "ok")
	})
	return addr, seen
}

// Scenario 10: forwarded headers are set from our view of the connection,
// client-supplied values are dropped, and Host is preserved.
func TestScenario10ForwardedHeaders(t *testing.T) {
	a, seen := capturingBackend(t)
	store := newStore(t, snapshot(
		[]model.Backend{backend(beA, a)},
		route("app", "app.example.com", "/", beA),
	))
	front := newFront(t, newHandler(t, store, Options{}))

	resp, body := get(t, front, "app.example.com", "/path", http.Header{
		"X-Forwarded-For":   {"1.2.3.4"},
		"X-Forwarded-Host":  {"evil.example.net"},
		"X-Forwarded-Proto": {"https"},
		"Forwarded":         {"for=1.2.3.4"},
		"X-Custom":          {"kept"},
	})
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("status %d body %q, want 200 ok", resp.StatusCode, body)
	}

	got := wait(t, seen, "backend request")
	if got.host != "app.example.com" {
		t.Errorf("backend Host = %q, want app.example.com (Ingress semantics)", got.host)
	}
	// The front server listens on loopback, so the only peer the proxy can
	// vouch for is 127.0.0.1. The client's 1.2.3.4 must not appear at all,
	// neither appended nor prepended.
	want := map[string][]string{
		"X-Forwarded-For":   {"127.0.0.1"},
		"X-Forwarded-Host":  {"app.example.com"},
		"X-Forwarded-Proto": {"http"},
		"Forwarded":         nil,
		"X-Custom":          {"kept"},
	}
	for name, values := range want {
		if g := got.header.Values(name); !equalStrings(g, values) {
			t.Errorf("backend %s = %v, want %v", name, g, values)
		}
	}
}

// Scenario 11 at the proxy level: request and response header templates are
// evaluated against the inbound request, a header variable copies the client
// header, and an empty evaluated value leaves the header unset.
func TestScenario11HeaderTemplatesEvaluated(t *testing.T) {
	a, seen := capturingBackend(t)
	r := route("app", "app.example.com", "/", beA)
	r.RequestHeaders = []model.HeaderRule{
		{Name: "X-Sluice-Route", Value: "{k8s.route}"},
		{Name: "X-Client-IP", Value: "{http.request.remote}"},
		{Name: "X-Copied", Value: "{http.request.header.X-Source}"},
		{Name: "X-Upstream", Value: "{upstream.address}"},
		{Name: "X-Static", Value: "fixed"},
		{Name: "X-Path", Value: "{http.request.path}"},
		{Name: "X-Mixed", Value: "{http.request.method} {k8s.namespace}/{k8s.service}"},
		// Deliberately settable: overrides what SetXForwarded wrote.
		{Name: "X-Forwarded-Proto", Value: "https"},
	}
	r.ResponseHeaders = []model.HeaderRule{
		{Name: "X-Served-By", Value: "{k8s.namespace}/{k8s.service}"},
		{Name: "X-Echo", Value: "{http.request.header.X-Source}"},
		{Name: "X-Resp-Upstream", Value: "{upstream.address}"},
		// Read from the inbound request: the outbound clone carries the
		// proxy's own X-Forwarded-For, which must not leak into templates.
		{Name: "X-Inbound-XFF", Value: "{http.request.header.X-Forwarded-For}"},
	}
	store := newStore(t, snapshot([]model.Backend{backend(beA, a)}, r))
	front := newFront(t, newHandler(t, store, Options{}))

	t.Run("with source header", func(t *testing.T) {
		resp, _ := get(t, front, "app.example.com", "/p%20q?ignored=1", http.Header{
			"X-Source":        {"abc"},
			"X-Forwarded-For": {"1.2.3.4"},
		})
		got := wait(t, seen, "backend request")
		wantRequest := map[string]string{
			"X-Sluice-Route":    "default/app",
			"X-Client-IP":       "127.0.0.1",
			"X-Copied":          "abc",
			"X-Upstream":        a,
			"X-Static":          "fixed",
			"X-Path":            "/p%20q",
			"X-Mixed":           "GET default/svc-a",
			"X-Forwarded-Proto": "https",
			"X-Forwarded-For":   "127.0.0.1",
		}
		for name, want := range wantRequest {
			if g := got.header.Get(name); g != want {
				t.Errorf("backend %s = %q, want %q", name, g, want)
			}
		}
		wantResponse := map[string]string{
			"X-Served-By":     "default/svc-a",
			"X-Echo":          "abc",
			"X-Resp-Upstream": a,
			"X-Inbound-XFF":   "1.2.3.4",
		}
		for name, want := range wantResponse {
			if g := resp.Header.Get(name); g != want {
				t.Errorf("response %s = %q, want %q", name, g, want)
			}
		}
	})

	t.Run("without source header", func(t *testing.T) {
		resp, _ := get(t, front, "app.example.com", "/", nil)
		got := wait(t, seen, "backend request")
		if _, present := got.header["X-Copied"]; present {
			t.Errorf("backend X-Copied = %v, want unset when the source header is absent", got.header["X-Copied"])
		}
		if g := got.header.Get("X-Static"); g != "fixed" {
			t.Errorf("backend X-Static = %q, want fixed", g)
		}
		for _, name := range []string{"X-Echo", "X-Inbound-XFF"} {
			if _, present := resp.Header[name]; present {
				t.Errorf("response %s = %v, want unset when the value is empty", name, resp.Header[name])
			}
		}
		if g := resp.Header.Get("X-Served-By"); g != "default/svc-a" {
			t.Errorf("response X-Served-By = %q, want default/svc-a", g)
		}
	})
}

// Request header rules replace client-supplied values of the same name (Set
// semantics), and the backend's own response headers survive the response
// rules.
func TestHeaderRulesSetSemantics(t *testing.T) {
	a, seen := capturingBackend(t)
	r := route("app", "app.example.com", "/", beA)
	r.RequestHeaders = []model.HeaderRule{{Name: "X-Tenant", Value: "{k8s.namespace}"}}
	r.ResponseHeaders = []model.HeaderRule{{Name: "X-Served-By", Value: "sluice"}}
	store := newStore(t, snapshot([]model.Backend{backend(beA, a)}, r))
	front := newFront(t, newHandler(t, store, Options{}))

	resp, _ := get(t, front, "app.example.com", "/", http.Header{"X-Tenant": {"spoofed", "twice"}})
	got := wait(t, seen, "backend request")
	if g := got.header.Values("X-Tenant"); !equalStrings(g, []string{"default"}) {
		t.Errorf("backend X-Tenant = %v, want [default]: the rule replaces client values", g)
	}
	if g := resp.Header.Get("X-Served-By"); g != "sluice" {
		t.Errorf("response X-Served-By = %q, want sluice", g)
	}
	if g := resp.Header.Get("Content-Type"); g == "" {
		t.Error("the backend's Content-Type did not survive the response rules")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
