// Package router compiles a model.Snapshot into an immutable routing Table and
// publishes tables atomically through a Store.
//
// Compile is the single place where configuration is validated. A Table never
// changes after Compile returns, except for the round-robin counters inside
// its pickers, so the data plane can read it without locks.
package router

import (
	"errors"
	"fmt"
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
	Key      model.RouteKey
	Host     string
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

	// TODO(wave-2, model+router agent): unexported matching structures:
	// exact hosts map; wildcard hosts map keyed by the literal "*.suffix"
	// (a wildcard matches exactly one label, so the only candidate key for a
	// request host is "*" + host[firstDot:]); catch-all for empty host. Per
	// host: an exact-path map and a prefix slice sorted by length descending.
	routes []*Route
}

// RouteError wraps a validation error with the route it concerns, so a log
// line can name the offending Ingress.
type RouteError struct {
	Key model.RouteKey
	Err error
}

func (e *RouteError) Error() string { return fmt.Sprintf("route %s: %v", e.Key, e.Err) }
func (e *RouteError) Unwrap() error { return e.Err }

// Compile validates s and builds a Table. It is the sole validator of the
// model. Rejected, each wrapped in a *RouteError: a path not starting with
// "/"; an invalid or non-lowercase host, or a wildcard not in the leftmost
// label; a duplicate (host, path, type) with the same route key; a backend
// key with no matching Backend; header rules failing
// model.ValidateHeaderRules or placeholders.ValidateHeaderName; a header
// value failing placeholders.Parse. Pickers are built from Ready endpoints
// only. Nothing is partially applied on error.
func Compile(s *model.Snapshot) (*Table, error) {
	// TODO(wave-2, model+router agent): implement.
	return nil, errors.New("router: not implemented")
}

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
	// TODO(wave-2, model+router agent): implement priority rules.
	return nil, false
}

// Len returns the number of routes in the table.
func (t *Table) Len() int { return len(t.routes) }

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
	// TODO(wave-2, model+router agent): implement (lock, Normalize, Compile,
	// hash compare, version++, stamp, cur.Store).
	return nil, false, errors.New("router: not implemented")
}
