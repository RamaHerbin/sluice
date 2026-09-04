package router

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/placeholders"
)

// oneBackend is the smallest valid backend list.
func oneBackend() []model.Backend {
	return []model.Backend{readyBackend(beA, "10.0.0.1:8080")}
}

// withRequestHeaders returns r with the given request header rules.
func withRequestHeaders(r model.Route, rules ...model.HeaderRule) model.Route {
	r.RequestHeaders = rules
	return r
}

// Scenario 12 (Compile level): an unknown placeholder in a header rule is a
// validation error naming the route.
func TestScenario12UnknownPlaceholderRejected(t *testing.T) {
	snap := &model.Snapshot{
		Backends: oneBackend(),
		Routes: []model.Route{
			newRoute("good", "", "/", model.PathPrefix, beA),
			withRequestHeaders(newRoute("bad", "", "/bad", model.PathPrefix, beA),
				model.HeaderRule{Name: "X-Where", Value: "{nope}"}),
		},
	}
	tbl, err := Compile(snap)
	if tbl != nil {
		t.Error("Compile returned a table together with an error")
	}
	re := wantRouteError(t, err, keyOf("bad"))
	var uv *placeholders.UnknownVariableError
	if !errors.As(err, &uv) {
		t.Fatalf("error %v does not wrap *placeholders.UnknownVariableError", err)
	}
	if uv.Name != "nope" {
		t.Errorf("UnknownVariableError.Name = %q, want %q", uv.Name, "nope")
	}
	if !strings.HasPrefix(re.Error(), "route default/bad#r0p0: ") {
		t.Errorf("RouteError message %q does not start with the route key", re.Error())
	}
}

func TestCompileRejections(t *testing.T) {
	tooMany := make([]model.HeaderRule, model.MaxHeaderRules+1)
	for i := range tooMany {
		tooMany[i] = model.HeaderRule{Name: "X-" + strings.Repeat("a", i+1), Value: "v"}
	}

	tests := []struct {
		name    string
		route   model.Route
		wantIs  error  // optional sentinel the error must wrap
		wantMsg string // optional substring of the message
	}{
		{
			name:    "path without leading slash",
			route:   newRoute("bad", "", "foo", model.PathPrefix, beA),
			wantMsg: "path must start with",
		},
		{
			name:    "empty path",
			route:   newRoute("bad", "", "", model.PathExact, beA),
			wantMsg: "path must start with",
		},
		{
			name:    "unknown path type",
			route:   newRoute("bad", "", "/", model.PathType(7), beA),
			wantMsg: "unknown path type",
		},
		{
			name:    "uppercase host",
			route:   newRoute("bad", "Example.com", "/", model.PathPrefix, beA),
			wantMsg: "lowercase",
		},
		{
			name:    "host with trailing dot",
			route:   newRoute("bad", "example.com.", "/", model.PathPrefix, beA),
			wantMsg: "empty DNS label",
		},
		{
			name:    "host with empty label",
			route:   newRoute("bad", "a..example.com", "/", model.PathPrefix, beA),
			wantMsg: "empty DNS label",
		},
		{
			name:    "host label starting with hyphen",
			route:   newRoute("bad", "-a.example.com", "/", model.PathPrefix, beA),
			wantMsg: "hyphen",
		},
		{
			name:    "host label ending with hyphen",
			route:   newRoute("bad", "a-.example.com", "/", model.PathPrefix, beA),
			wantMsg: "hyphen",
		},
		{
			name:    "host with underscore",
			route:   newRoute("bad", "a_b.example.com", "/", model.PathPrefix, beA),
			wantMsg: "invalid byte",
		},
		{
			name:    "host with port",
			route:   newRoute("bad", "example.com:8080", "/", model.PathPrefix, beA),
			wantMsg: "invalid byte",
		},
		{
			name:    "host label too long",
			route:   newRoute("bad", strings.Repeat("a", 64)+".example.com", "/", model.PathPrefix, beA),
			wantMsg: "max 63",
		},
		{
			name: "host too long",
			route: newRoute("bad", strings.Repeat(strings.Repeat("a", 63)+".", 4)+"example.com",
				"/", model.PathPrefix, beA),
			wantMsg: "max 253",
		},
		{
			name:    "wildcard alone",
			route:   newRoute("bad", "*", "/", model.PathPrefix, beA),
			wantMsg: "wildcard",
		},
		{
			name:    "wildcard without suffix",
			route:   newRoute("bad", "*.", "/", model.PathPrefix, beA),
			wantMsg: "wildcard",
		},
		{
			name:    "wildcard not leftmost",
			route:   newRoute("bad", "a.*.example.com", "/", model.PathPrefix, beA),
			wantMsg: "leftmost",
		},
		{
			name:    "wildcard glued to a label",
			route:   newRoute("bad", "*example.com", "/", model.PathPrefix, beA),
			wantMsg: "leftmost",
		},
		{
			name:    "wildcard as rightmost label",
			route:   newRoute("bad", "example.*", "/", model.PathPrefix, beA),
			wantMsg: "leftmost",
		},
		{
			name:    "two wildcards",
			route:   newRoute("bad", "*.*.example.com", "/", model.PathPrefix, beA),
			wantMsg: "leftmost",
		},
		{
			name:    "missing backend",
			route:   newRoute("bad", "", "/", model.PathPrefix, beB),
			wantMsg: "no such backend",
		},
		{
			name: "too many header rules",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				tooMany...),
			wantMsg: "header rules, max",
		},
		{
			name: "empty header name",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "", Value: "v"}),
			wantMsg: "empty header name",
		},
		{
			name: "empty header value",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "X-A", Value: ""}),
			wantMsg: "empty value",
		},
		{
			name: "duplicate header names differing in case",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "X-A", Value: "1"},
				model.HeaderRule{Name: "x-a", Value: "2"}),
			wantMsg: "duplicate",
		},
		{
			name: "header name is not a token",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "Bad Name", Value: "v"}),
			wantMsg: "token",
		},
		{
			name: "denied header name",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "host", Value: "v"}),
			wantMsg: "reserved",
		},
		{
			name: "unbalanced braces in template",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "X-A", Value: "{k8s.route"}),
			wantIs: placeholders.ErrUnbalancedBraces,
		},
		{
			name: "template too long",
			route: withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "X-A", Value: strings.Repeat("a", placeholders.MaxTemplateLength+1)}),
			wantIs: placeholders.ErrTemplateTooLong,
		},
		{
			name: "unknown placeholder in a response header",
			route: func() model.Route {
				r := newRoute("bad", "", "/", model.PathPrefix, beA)
				r.ResponseHeaders = []model.HeaderRule{{Name: "X-A", Value: "{nope}"}}
				return r
			}(),
			wantMsg: "response headers",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := &model.Snapshot{
				Backends: oneBackend(),
				Routes: []model.Route{
					newRoute("good", "", "/good", model.PathPrefix, beA),
					tc.route,
				},
			}
			tbl, err := Compile(snap)
			if tbl != nil {
				t.Error("Compile returned a table together with an error")
			}
			_ = wantRouteError(t, err, keyOf("bad"))
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestCompileRejectionNeverEchoesHeaderValue: a header value can be a secret,
// so a rejection message must not contain it.
func TestCompileRejectionNeverEchoesHeaderValue(t *testing.T) {
	const secret = "s3cr3t-value"
	snap := &model.Snapshot{
		Backends: oneBackend(),
		Routes: []model.Route{
			withRequestHeaders(newRoute("bad", "", "/", model.PathPrefix, beA),
				model.HeaderRule{Name: "Bad Name", Value: secret}),
		},
	}
	_, err := Compile(snap)
	_ = wantRouteError(t, err, keyOf("bad"))
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error %q echoes the header value", err.Error())
	}
}

func TestCompileDuplicateRouteSameKeyRejected(t *testing.T) {
	tests := []struct {
		name string
		a, b model.Route
	}{
		{
			name: "identical routes",
			a:    newRoute("dup", "example.com", "/x", model.PathPrefix, beA),
			b:    newRoute("dup", "example.com", "/x", model.PathPrefix, beA),
		},
		{
			name: "prefixes trimming to the same path",
			a:    newRoute("dup", "", "/x", model.PathPrefix, beA),
			b:    newRoute("dup", "", "/x/", model.PathPrefix, beA),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(&model.Snapshot{Backends: oneBackend(), Routes: []model.Route{tc.a, tc.b}})
			_ = wantRouteError(t, err, keyOf("dup"))
			if !strings.Contains(err.Error(), "duplicate route") {
				t.Errorf("error %q does not say duplicate route", err.Error())
			}
		})
	}
}

// TestCompileSameKeyDifferentTypeAccepted: Exact and Prefix on the same path
// are two different routes even under one key.
func TestCompileSameKeyDifferentTypeAccepted(t *testing.T) {
	tbl := mustCompile(t, &model.Snapshot{
		Backends: oneBackend(),
		Routes: []model.Route{
			newRoute("same", "", "/x", model.PathPrefix, beA),
			newRoute("same", "", "/x", model.PathExact, beA),
		},
	})
	if tbl.Len() != 2 {
		t.Errorf("Len() = %d, want 2", tbl.Len())
	}
}

func TestCompileDuplicateBackendKeyRejected(t *testing.T) {
	_, err := Compile(&model.Snapshot{
		Backends: []model.Backend{readyBackend(beA, "10.0.0.1:8080"), readyBackend(beA, "10.0.0.2:8080")},
	})
	if err == nil {
		t.Fatal("Compile accepted a duplicate backend key")
	}
	if !strings.Contains(err.Error(), "duplicate backend key") {
		t.Errorf("error %q does not say duplicate backend key", err.Error())
	}
}

// TestCompileConflictOldestWins: the same (host, path, type) from different
// Ingresses is not an error; the oldest Created wins and the others are
// shadowed but still counted.
func TestCompileConflictOldestWins(t *testing.T) {
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	created := func(r model.Route, d time.Duration) model.Route {
		r.Created = base.Add(d)
		return r
	}
	for _, typ := range []model.PathType{model.PathPrefix, model.PathExact} {
		t.Run(typ.String(), func(t *testing.T) {
			snap := &model.Snapshot{
				Backends: []model.Backend{readyBackend(beA, "10.0.0.1:8080"), readyBackend(beB, "10.0.0.2:8080")},
				Routes: []model.Route{
					created(newRoute("newer", "example.com", "/x", typ, beB), 2*time.Hour),
					created(newRoute("oldest", "example.com", "/x", typ, beA), 0),
					created(newRoute("middle", "example.com", "/x", typ, beB), time.Hour),
				},
			}
			tbl := mustCompile(t, snap)
			if tbl.Len() != 3 {
				t.Errorf("Len() = %d, want 3 (shadowed routes are counted)", tbl.Len())
			}
			r, ok := tbl.Match("example.com", "/x")
			if !ok {
				t.Fatal("Match found nothing")
			}
			if r.Key.Name != "oldest" || r.Service != "svc-a" {
				t.Errorf("Match = %s (%s), want oldest (svc-a)", r.Key.Name, r.Service)
			}
		})
	}
}

// TestCompileConflictTieBreakByNamespaceName: equal Created falls back to
// the lexical order of Namespace then Name.
func TestCompileConflictTieBreakByNamespaceName(t *testing.T) {
	same := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	mk := func(ns, name string) model.Route {
		r := newRoute(name, "", "/x", model.PathPrefix, beA)
		r.Key.Namespace = ns
		r.Created = same
		return r
	}
	tbl := mustCompile(t, &model.Snapshot{
		Backends: oneBackend(),
		Routes: []model.Route{
			mk("zeta", "aaa"),
			mk("alpha", "zzz"),
			mk("alpha", "bbb"),
		},
	})
	r, ok := tbl.Match("", "/x/y")
	if !ok {
		t.Fatal("Match found nothing")
	}
	if r.Key.Namespace != "alpha" || r.Key.Name != "bbb" {
		t.Errorf("Match = %s, want alpha/bbb", r.Key)
	}
}

// TestCompileConflictIndependentOfOrder: the winner does not depend on the
// order routes appear in the snapshot.
func TestCompileConflictIndependentOfOrder(t *testing.T) {
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	older := newRoute("older", "", "/x", model.PathPrefix, beA)
	older.Created = base
	newer := newRoute("newer", "", "/x", model.PathPrefix, beA)
	newer.Created = base.Add(time.Minute)
	for _, routes := range [][]model.Route{{older, newer}, {newer, older}} {
		tbl := mustCompile(t, &model.Snapshot{Backends: oneBackend(), Routes: routes})
		r, ok := tbl.Match("", "/x")
		if !ok || r.Key.Name != "older" {
			t.Errorf("Match = %v (%v), want older", r, ok)
		}
	}
}

func TestCompilePickersUseReadyEndpointsOnly(t *testing.T) {
	mixed := model.Backend{Key: beA, Endpoints: []model.Endpoint{
		{Address: "10.0.0.3:8080", Ready: true},
		{Address: "10.0.0.2:8080", Ready: false},
		{Address: "10.0.0.1:8080", Ready: true},
		{Address: "10.0.0.1:8080", Ready: true},
	}}
	noneReady := model.Backend{Key: beB, Endpoints: []model.Endpoint{
		{Address: "10.0.1.1:8080", Ready: false},
	}}
	empty := model.Backend{Key: model.BackendKey{Namespace: "default", Service: "svc-c", Port: 80}}
	tbl := mustCompile(t, &model.Snapshot{
		Backends: []model.Backend{mixed, noneReady, empty},
		Routes: []model.Route{
			newRoute("mixed", "", "/mixed", model.PathPrefix, beA),
			newRoute("none", "", "/none", model.PathPrefix, beB),
			newRoute("empty", "", "/empty", model.PathPrefix, empty.Key),
		},
	})

	r, ok := tbl.Match("", "/mixed")
	if !ok {
		t.Fatal("Match(/mixed) found nothing")
	}
	if r.Backend.Len() != 2 {
		t.Errorf("mixed picker Len() = %d, want 2 (ready, deduplicated)", r.Backend.Len())
	}
	// Sorted addresses, round-robin from the first one.
	want := []string{"10.0.0.1:8080", "10.0.0.3:8080", "10.0.0.1:8080"}
	for i, w := range want {
		if addr, ok := r.Backend.Pick(); !ok || addr != w {
			t.Errorf("Pick %d = (%q, %v), want (%q, true)", i, addr, ok, w)
		}
	}

	for _, path := range []string{"/none", "/empty"} {
		r, ok := tbl.Match("", path)
		if !ok {
			t.Fatalf("Match(%s) found nothing", path)
		}
		if r.Backend == nil {
			t.Fatalf("Match(%s).Backend is nil", path)
		}
		if r.Backend.Len() != 0 {
			t.Errorf("Match(%s).Backend.Len() = %d, want 0", path, r.Backend.Len())
		}
		if addr, ok := r.Backend.Pick(); ok || addr != "" {
			t.Errorf("Match(%s).Backend.Pick() = (%q, %v), want (\"\", false)", path, addr, ok)
		}
	}
}

func TestCompileRouteFields(t *testing.T) {
	be := model.BackendKey{Namespace: "shop", Service: "cart", Port: 8080}
	r := newRoute("r", "shop.example.com", "/cart/", model.PathPrefix, be)
	r.Key = model.RouteKey{Namespace: "shop", Name: "cart-ingress", Rule: 1, Path: 2}
	r.RequestHeaders = []model.HeaderRule{
		{Name: "X-Static", Value: "fixed"},
		{Name: "X-Route", Value: "{k8s.namespace}/{k8s.service}"},
	}
	r.ResponseHeaders = []model.HeaderRule{{Name: "X-Served-By", Value: "sluice"}}
	tbl := mustCompile(t, &model.Snapshot{
		Backends: []model.Backend{readyBackend(be, "10.1.0.1:8080")},
		Routes:   []model.Route{r},
	})
	if tbl.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", tbl.Len())
	}
	if tbl.Version != 0 || tbl.Hash != "" {
		t.Errorf("Compile stamped Version=%d Hash=%q, want zero values outside a Store", tbl.Version, tbl.Hash)
	}
	if tbl.BuiltAt.IsZero() {
		t.Error("BuiltAt is zero")
	}
	got, ok := tbl.Match("shop.example.com", "/cart/items")
	if !ok {
		t.Fatal("Match found nothing")
	}
	if got.Key != r.Key {
		t.Errorf("Key = %v, want %v", got.Key, r.Key)
	}
	if got.Host != "shop.example.com" || got.Path != "/cart" || got.PathType != model.PathPrefix {
		t.Errorf("Host/Path/PathType = %q %q %s, want shop.example.com /cart Prefix", got.Host, got.Path, got.PathType)
	}
	if got.Namespace != "shop" || got.Service != "cart" {
		t.Errorf("Namespace/Service = %q/%q, want shop/cart", got.Namespace, got.Service)
	}
	if len(got.RequestHeaders) != 2 || len(got.ResponseHeaders) != 1 {
		t.Fatalf("headers = %d request, %d response, want 2 and 1", len(got.RequestHeaders), len(got.ResponseHeaders))
	}
	if h := got.RequestHeaders[0]; h.Name != "X-Static" || !h.Value.IsStatic() {
		t.Errorf("RequestHeaders[0] = %q static=%v, want X-Static static", h.Name, h.Value.IsStatic())
	}
	h := got.RequestHeaders[1]
	if h.Name != "X-Route" || h.Value.IsStatic() {
		t.Errorf("RequestHeaders[1] = %q static=%v, want X-Route dynamic", h.Name, h.Value.IsStatic())
	}
	ctx := placeholders.Context{Namespace: got.Namespace, Service: got.Service}
	if v := h.Value.Evaluate(ctx); v != "shop/cart" {
		t.Errorf("X-Route evaluates to %q, want shop/cart", v)
	}
}

func TestCompilePathStorage(t *testing.T) {
	tests := []struct {
		path string
		typ  model.PathType
		want string
	}{
		{"/", model.PathPrefix, "/"},
		{"/foo", model.PathPrefix, "/foo"},
		{"/foo/", model.PathPrefix, "/foo"},
		{"/foo//", model.PathPrefix, "/foo/"},
		{"/", model.PathExact, "/"},
		{"/foo/", model.PathExact, "/foo/"},
	}
	for _, tc := range tests {
		tbl := mustCompile(t, &model.Snapshot{
			Backends: oneBackend(),
			Routes:   []model.Route{newRoute("r", "", tc.path, tc.typ, beA)},
		})
		if got := tbl.routes[0].Path; got != tc.want {
			t.Errorf("%s %q stored as %q, want %q", tc.typ, tc.path, got, tc.want)
		}
	}
}

func TestCompileNilAndEmptySnapshot(t *testing.T) {
	for name, snap := range map[string]*model.Snapshot{"nil": nil, "empty": {}} {
		tbl, err := Compile(snap)
		if err != nil {
			t.Fatalf("Compile(%s) error: %v", name, err)
		}
		if tbl.Len() != 0 {
			t.Errorf("Compile(%s).Len() = %d, want 0", name, tbl.Len())
		}
		if _, ok := tbl.Match("example.com", "/"); ok {
			t.Errorf("Compile(%s).Match succeeded on an empty table", name)
		}
	}
}

func TestCompileAcceptsValidHosts(t *testing.T) {
	hosts := []string{
		"",
		"localhost",
		"example.com",
		"a-b.example.com",
		"1.2.3.4",
		"123.example.com",
		"*.example.com",
		"*.a",
		strings.Repeat("a", 63) + ".example.com",
	}
	for _, host := range hosts {
		if _, err := Compile(&model.Snapshot{
			Backends: oneBackend(),
			Routes:   []model.Route{newRoute("r", host, "/", model.PathPrefix, beA)},
		}); err != nil {
			t.Errorf("host %q rejected: %v", host, err)
		}
	}
}

// TestCompileDoesNotMutateSnapshot: Compile may be called on a snapshot the
// caller still owns; the trimmed path must not leak back into it.
func TestCompileDoesNotMutateSnapshot(t *testing.T) {
	snap := &model.Snapshot{
		Backends: oneBackend(),
		Routes:   []model.Route{newRoute("r", "", "/foo/", model.PathPrefix, beA)},
	}
	mustCompile(t, snap)
	if snap.Routes[0].Path != "/foo/" {
		t.Errorf("Compile changed the snapshot path to %q", snap.Routes[0].Path)
	}
}
