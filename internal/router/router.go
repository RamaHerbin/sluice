// Package router compiles a model.Snapshot into an immutable routing Table and
// publishes tables atomically through a Store.
//
// Compile is the single place where configuration is validated. A Table never
// changes after Compile returns, except for the round-robin counters inside
// its pickers, so the data plane can read it without locks.
package router

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/placeholders"
	"github.com/RamaHerbin/sluice/internal/upstream"
)

// Header is a compiled header rule.
type Header struct {
	Name  string
	Value placeholders.Template
}

// Route is a compiled route, ready to serve requests.
type Route struct {
	Key  model.RouteKey
	Host string
	// Path is the matching path. For a Prefix route one trailing slash has
	// been trimmed ("/foo/" is stored as "/foo"), except for "/" itself. An
	// Exact path is stored as given.
	Path     string
	PathType model.PathType
	// Namespace and Service identify the backend Service and feed the
	// {k8s.namespace} and {k8s.service} placeholders.
	Namespace string
	Service   string
	// Backend is never nil. Len()==0 means the route has no ready endpoint
	// and the proxy answers 503.
	Backend         upstream.Picker
	RequestHeaders  []Header
	ResponseHeaders []Header
}

// Table is an immutable compiled routing table.
type Table struct {
	// Version is monotonic per Store and 0 for tables built outside a Store.
	Version uint64
	// Hash identifies the normalized snapshot the table was built from.
	Hash    string
	BuiltAt time.Time

	// routes holds every compiled route, including a route shadowed by a
	// conflicting one (same host, path and type from another Ingress), so
	// Len reports what the snapshot contained.
	routes []*Route
	// exact maps a lowercase host to its routes.
	exact map[string]*hostRoutes
	// wildcard maps the literal route host "*.suffix" to its routes. A
	// wildcard matches exactly one label, so the only candidate key for a
	// request host is "*" + host[firstDot:].
	wildcard map[string]*hostRoutes
	// catchAll holds the routes with an empty host, nil when there are none.
	catchAll *hostRoutes
}

// hostRoutes is the set of routes sharing one host value.
type hostRoutes struct {
	// exact maps an Exact path to its route.
	exact map[string]*Route
	// prefixes holds the Prefix routes sorted by path length descending, so
	// a linear scan finds the longest matching prefix first. Ties in length
	// are ordered by creation time then key for determinism only: two
	// distinct prefixes of equal length never both match one path.
	prefixes []*Route
}

// RouteError wraps a validation error with the route it concerns, so a log
// line can name the offending Ingress.
type RouteError struct {
	Key model.RouteKey
	Err error
}

func (e *RouteError) Error() string { return fmt.Sprintf("route %s: %v", e.Key, e.Err) }
func (e *RouteError) Unwrap() error { return e.Err }

// Empty returns a table with no routes. Every Match on it reports false.
func Empty() *Table {
	return &Table{BuiltAt: time.Now()}
}

// Match returns the best route for host and path, or false when none matches.
//
// Host normalization: the ":port" suffix is removed when net.SplitHostPort
// accepts the value, one trailing dot is removed, and the result is
// lowercased. A bracketed IPv6 literal keeps its brackets and therefore only
// matches empty-host routes, which is fine because an Ingress host is never
// an IP address. The path is matched as received (no percent-decoding, no
// dot-segment cleanup).
//
// Priority: exact host, then wildcard host, then empty host, with no
// fallthrough to a less specific host when its paths do not match. Within a
// host: an exact-path hit wins, otherwise the longest matching prefix. This
// is equivalent to "longest path, Exact beats Prefix at equal length"
// because an Exact match always spans the whole request path.
func (t *Table) Match(host, path string) (*Route, bool) {
	h := normalizeHost(host)
	if h != "" {
		// Lookups on a nil map are fine, so Empty() needs no special case.
		if hr, ok := t.exact[h]; ok {
			return hr.match(path)
		}
		// The leftmost label must be non-empty for a wildcard to match it,
		// hence dot > 0 rather than dot >= 0.
		if dot := strings.IndexByte(h, '.'); dot > 0 {
			if hr, ok := t.wildcard["*"+h[dot:]]; ok {
				return hr.match(path)
			}
		}
	}
	if t.catchAll != nil {
		return t.catchAll.match(path)
	}
	return nil, false
}

// Len returns the number of compiled routes, including routes shadowed by a
// conflicting route with an older Created (they are kept for logging).
func (t *Table) Len() int { return len(t.routes) }

// normalizeHost applies the Match host normalization: port stripped when
// net.SplitHostPort accepts the value, one trailing dot removed, lowercased.
func normalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// match finds the best route for path within one host: the Exact route for
// the whole path when there is one, otherwise the longest matching Prefix.
func (hr *hostRoutes) match(path string) (*Route, bool) {
	if r, ok := hr.exact[path]; ok {
		return r, true
	}
	for _, r := range hr.prefixes {
		if prefixMatches(r.Path, path) {
			return r, true
		}
	}
	return nil, false
}

// prefixMatches reports whether path is element-wise under prefix, where
// prefix has already had its trailing slash trimmed: "/foo" matches "/foo",
// "/foo/" and "/foo/bar" but not "/foobar". The prefix "/" matches every
// path.
func prefixMatches(prefix, path string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// Store publishes tables atomically and keeps the last valid one.
//
// Readers call Current without locking. Writers are serialized by a mutex so
// versions are monotonic. Apply swaps the table only when Compile succeeds,
// which is the whole "last known good" mechanism.
type Store struct {
	mu      sync.Mutex
	cur     atomic.Pointer[Table]
	version uint64
}

// NewStore returns a Store holding Empty() at version 0, so consumers never
// observe a nil table.
func NewStore() *Store {
	s := &Store{}
	s.cur.Store(Empty())
	return s
}

// Current returns the current table. It never returns nil.
func (s *Store) Current() *Table { return s.cur.Load() }

// Apply normalizes and compiles snap, then publishes the result. On error the
// current table is unchanged. changed is false when the new snapshot hashes
// identically to the current table.
func (s *Store) Apply(snap *model.Snapshot) (t *Table, changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Normalize before both Compile and Hash: the hash covers slice order,
	// and Compile's tie-breaks must not depend on the producer's order.
	snap.Normalize()
	t, err = Compile(snap)
	if err != nil {
		// Nothing is published. The error already names the route.
		return nil, false, err
	}

	hash := snap.Hash()
	cur := s.cur.Load()
	if cur.Hash == hash {
		return cur, false, nil
	}

	s.version++
	t.Version = s.version
	t.Hash = hash
	t.BuiltAt = time.Now()
	s.cur.Store(t)
	return t, true, nil
}
