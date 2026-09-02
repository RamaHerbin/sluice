package router

import (
	"testing"

	"github.com/RamaHerbin/sluice/internal/model"
)

// matchTable builds the table used by TestMatchRules. Route names are the
// labels the cases refer to.
func matchTable(t *testing.T) *Table {
	t.Helper()
	snap := &model.Snapshot{
		Backends: []model.Backend{readyBackend(beA, "10.0.0.1:8080")},
		Routes: []model.Route{
			// Empty-host bucket.
			newRoute("root", "", "/", model.PathPrefix, beA),
			newRoute("foo", "", "/foo", model.PathPrefix, beA),
			newRoute("foo-bar-prefix", "", "/foo/bar", model.PathPrefix, beA),
			newRoute("foo-bar-exact", "", "/foo/bar", model.PathExact, beA),
			newRoute("trail", "", "/trail/", model.PathPrefix, beA),
			newRoute("case-exact", "", "/Case", model.PathExact, beA),
			newRoute("slash-exact", "", "/slash/", model.PathExact, beA),
			newRoute("slash-prefix", "", "/slash", model.PathPrefix, beA),
			// Exact hosts.
			newRoute("apex", "example.com", "/", model.PathPrefix, beA),
			newRoute("a", "a.example.com", "/", model.PathPrefix, beA),
			newRoute("strict-x", "strict.example.com", "/x", model.PathPrefix, beA),
			// Wildcard hosts.
			newRoute("wild", "*.example.com", "/", model.PathPrefix, beA),
			newRoute("wild-test", "*.wild.test", "/", model.PathPrefix, beA),
			newRoute("narrow-n", "*.narrow.test", "/n", model.PathPrefix, beA),
		},
	}
	return mustCompile(t, snap)
}

// TestMatchRules encodes every row of docs/design/dataplane.md section 6 and
// docs/design/kubernetes.md section 4. want is the route name, or "" for no
// match.
func TestMatchRules(t *testing.T) {
	tbl := matchTable(t)
	tests := []struct {
		name string
		host string
		path string
		want string
	}{
		// Prefix is element-wise.
		{"prefix matches itself", "", "/foo", "foo"},
		{"prefix matches itself with trailing slash", "", "/foo/", "foo"},
		{"prefix matches a child", "", "/foo/baz", "foo"},
		{"prefix does not match a longer label", "", "/foobar", "root"},
		{"child with a longer label falls back to the parent prefix", "", "/foo/barbaz", "foo"},
		{"nested prefix matches its child", "", "/foo/bar/baz", "foo-bar-prefix"},
		{"exact beats prefix at equal length", "", "/foo/bar", "foo-bar-exact"},
		// Trailing slash of a Prefix is trimmed.
		{"trimmed prefix matches without slash", "", "/trail", "trail"},
		{"trimmed prefix matches with slash", "", "/trail/", "trail"},
		{"trimmed prefix matches a child", "", "/trail/x", "trail"},
		{"trimmed prefix does not match a longer label", "", "/trailer", "root"},
		// Exact is case-sensitive with a significant trailing slash.
		{"exact matches its own case", "", "/Case", "case-exact"},
		{"exact does not match another case", "", "/case", "root"},
		{"exact does not match with an extra trailing slash", "", "/Case/", "root"},
		{"exact with trailing slash matches only itself", "", "/slash/", "slash-exact"},
		{"prefix takes the path without the slash", "", "/slash", "slash-prefix"},
		{"prefix takes the child of the exact path", "", "/slash/x", "slash-prefix"},
		// Root prefix.
		{"root prefix catches any path", "", "/anything/else", "root"},
		{"root prefix catches an empty path", "", "", "root"},
		// Host normalization.
		{"host with port", "Example.COM:8080", "/", "apex"},
		{"uppercase host with port", "A.Example.Com:443", "/", "a"},
		{"host with a trailing dot", "example.com.", "/", "apex"},
		{"host with a trailing dot and port", "example.com.:80", "/", "apex"},
		{"empty request host uses the empty bucket", "", "/foo", "foo"},
		{"unknown host uses the empty bucket", "other.test", "/foo", "foo"},
		// IPv6 literals only reach the empty bucket.
		{"ipv6 literal with port", "[::1]:8080", "/foo", "foo"},
		{"bare ipv6 literal", "[::1]", "/foo", "foo"},
		// Wildcard hosts match exactly one label.
		{"wildcard matches one label", "b.example.com", "/", "wild"},
		{"wildcard does not match the apex", "wild.test", "/", "root"},
		{"wildcard does not match two labels", "a.b.wild.test", "/", "root"},
		{"wildcard does not match an empty leftmost label", ".example.com", "/", "root"},
		// Host priority and no fallthrough.
		{"exact host beats wildcard", "a.example.com", "/", "a"},
		{"exact host apex is not shadowed by wildcard", "example.com", "/", "apex"},
		{"exact host path matches", "strict.example.com", "/x", "strict-x"},
		{"no fallthrough from exact host to wildcard or empty", "strict.example.com", "/y", ""},
		{"wildcard host path matches", "a.narrow.test", "/n/x", "narrow-n"},
		{"no fallthrough from wildcard host to empty", "a.narrow.test", "/other", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := tbl.Match(tc.host, tc.path)
			if tc.want == "" {
				if ok {
					t.Fatalf("Match(%q, %q) = %s, want no match", tc.host, tc.path, r.Key.Name)
				}
				return
			}
			if !ok {
				t.Fatalf("Match(%q, %q) found nothing, want %s", tc.host, tc.path, tc.want)
			}
			if r.Key.Name != tc.want {
				t.Errorf("Match(%q, %q) = %s, want %s", tc.host, tc.path, r.Key.Name, tc.want)
			}
		})
	}
}

// TestMatchWithoutEmptyHostRoutes: with no empty-host routes an unknown host
// is a miss, and a matched host bucket still does not fall through.
func TestMatchWithoutEmptyHostRoutes(t *testing.T) {
	tbl := mustCompile(t, &model.Snapshot{
		Backends: []model.Backend{readyBackend(beA, "10.0.0.1:8080")},
		Routes: []model.Route{
			newRoute("apex", "example.com", "/", model.PathPrefix, beA),
			newRoute("wild-only", "*.example.com", "/only", model.PathPrefix, beA),
		},
	})
	tests := []struct {
		host, path, want string
	}{
		{"unknown.test", "/", ""},
		{"", "/", ""},
		{"a.example.com", "/", ""},
		{"a.example.com", "/only/x", "wild-only"},
		{"example.com", "/whatever", "apex"},
	}
	for _, tc := range tests {
		r, ok := tbl.Match(tc.host, tc.path)
		switch {
		case tc.want == "" && ok:
			t.Errorf("Match(%q, %q) = %s, want no match", tc.host, tc.path, r.Key.Name)
		case tc.want != "" && !ok:
			t.Errorf("Match(%q, %q) found nothing, want %s", tc.host, tc.path, tc.want)
		case tc.want != "" && r.Key.Name != tc.want:
			t.Errorf("Match(%q, %q) = %s, want %s", tc.host, tc.path, r.Key.Name, tc.want)
		}
	}
}

func TestEmptyTableMatchesNothing(t *testing.T) {
	tbl := Empty()
	if tbl.Len() != 0 {
		t.Errorf("Empty().Len() = %d, want 0", tbl.Len())
	}
	for _, host := range []string{"", "example.com", "a.example.com:8080"} {
		if r, ok := tbl.Match(host, "/"); ok {
			t.Errorf("Empty().Match(%q, \"/\") = %v, want no match", host, r.Key)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Example.COM:8080", "example.com"},
		{"EXAMPLE.com", "example.com"},
		{"example.com.", "example.com"},
		{"example.com..", "example.com."},
		{"example.com.:80", "example.com"},
		{"example.com:", "example.com"},
		{"[::1]:8080", "::1"},
		{"[::1]", "[::1]"},
		{"::1", "::1"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := normalizeHost(tc.in); got != tc.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Scenario 1: a request reaches the right service according to its hostname.
func TestScenario01HostSelectsService(t *testing.T) {
	tbl := mustCompile(t, &model.Snapshot{
		Backends: []model.Backend{
			readyBackend(beA, "10.0.0.1:8080"),
			readyBackend(beB, "10.0.0.2:8080"),
		},
		Routes: []model.Route{
			newRoute("app-a", "a.example.com", "/", model.PathPrefix, beA),
			newRoute("app-b", "b.example.com", "/", model.PathPrefix, beB),
		},
	})
	tests := []struct{ host, service, addr string }{
		{"a.example.com", "svc-a", "10.0.0.1:8080"},
		{"b.example.com", "svc-b", "10.0.0.2:8080"},
		{"B.example.com:8080", "svc-b", "10.0.0.2:8080"},
	}
	for _, tc := range tests {
		r, ok := tbl.Match(tc.host, "/index.html")
		if !ok {
			t.Fatalf("Match(%q) found nothing", tc.host)
		}
		if r.Service != tc.service || r.Namespace != "default" {
			t.Errorf("Match(%q) -> %s/%s, want default/%s", tc.host, r.Namespace, r.Service, tc.service)
		}
		if addr, ok := r.Backend.Pick(); !ok || addr != tc.addr {
			t.Errorf("Match(%q).Backend.Pick() = (%q, %v), want (%q, true)", tc.host, addr, ok, tc.addr)
		}
	}
	if _, ok := tbl.Match("c.example.com", "/"); ok {
		t.Error("Match for an unknown host succeeded with no empty-host route")
	}
}

// Scenario 2: the most specific path wins.
func TestScenario02MostSpecificPathWins(t *testing.T) {
	tbl := mustCompile(t, &model.Snapshot{
		Backends: []model.Backend{readyBackend(beA, "10.0.0.1:8080")},
		Routes: []model.Route{
			newRoute("root", "example.com", "/", model.PathPrefix, beA),
			newRoute("api", "example.com", "/api", model.PathPrefix, beA),
			newRoute("api-v1", "example.com", "/api/v1", model.PathPrefix, beA),
			newRoute("health", "example.com", "/api/v1/health", model.PathExact, beA),
		},
	})
	tests := []struct{ path, want string }{
		{"/", "root"},
		{"/about", "root"},
		{"/api", "api"},
		{"/api/", "api"},
		{"/api/v2", "api"},
		{"/apix", "root"},
		{"/api/v1", "api-v1"},
		{"/api/v1/users", "api-v1"},
		{"/api/v1/health", "health"},
		{"/api/v1/health/", "api-v1"},
		{"/api/v1/healthz", "api-v1"},
	}
	for _, tc := range tests {
		r, ok := tbl.Match("example.com", tc.path)
		if !ok {
			t.Errorf("Match(%q) found nothing, want %s", tc.path, tc.want)
			continue
		}
		if r.Key.Name != tc.want {
			t.Errorf("Match(%q) = %s, want %s", tc.path, r.Key.Name, tc.want)
		}
	}
}
