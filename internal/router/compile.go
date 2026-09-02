package router

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/placeholders"
	"github.com/RamaHerbin/sluice/internal/upstream"
)

// DNS limits from RFC 1123 as applied by Kubernetes to Ingress hosts.
const (
	maxHostLength  = 253
	maxLabelLength = 63
)

// Compile validates s and builds a Table. It is the sole validator of the
// model. Rejected, each wrapped in a *RouteError: a path not starting with
// "/"; an invalid or non-lowercase host, or a wildcard not in the leftmost
// label; a duplicate (host, path, type) with the same route key; a backend
// key with no matching Backend; header rules failing
// model.ValidateHeaderRules or placeholders.ValidateHeaderName; a header
// value failing placeholders.Parse. Pickers are built from Ready endpoints
// only. Nothing is partially applied on error.
//
// Two routes with the same (host, path, type) but different keys are not an
// error: they come from different Ingresses (or different rules of one
// Ingress) and the oldest Created wins, then the lexically smallest
// Namespace/Name. The loser is kept in Len but is never returned by Match.
// A duplicate Backend key is a producer bug and is rejected with a plain
// error, since no route can be blamed for it.
//
// A nil snapshot compiles to a table with no routes.
func Compile(s *model.Snapshot) (*Table, error) {
	if s == nil {
		s = &model.Snapshot{}
	}

	pickers, err := buildPickers(s.Backends)
	if err != nil {
		return nil, err
	}

	b := newBuilder()
	for i := range s.Routes {
		mr := &s.Routes[i]
		r, err := compileRoute(mr, pickers)
		if err != nil {
			return nil, &RouteError{Key: mr.Key, Err: err}
		}
		if err := b.add(r, mr.Created); err != nil {
			return nil, &RouteError{Key: mr.Key, Err: err}
		}
	}
	return b.table(), nil
}

// buildPickers returns one round-robin picker per backend, over its ready
// endpoints only, addresses sorted and deduplicated.
func buildPickers(backends []model.Backend) (map[model.BackendKey]upstream.Picker, error) {
	pickers := make(map[model.BackendKey]upstream.Picker, len(backends))
	for i := range backends {
		be := &backends[i]
		if _, dup := pickers[be.Key]; dup {
			return nil, fmt.Errorf("backend %s: duplicate backend key", be.Key)
		}
		addrs := make([]string, 0, len(be.Endpoints))
		for _, ep := range be.Endpoints {
			if ep.Ready {
				addrs = append(addrs, ep.Address)
			}
		}
		slices.Sort(addrs)
		addrs = slices.Compact(addrs)
		pickers[be.Key] = upstream.NewRoundRobin(addrs)
	}
	return pickers, nil
}

// compileRoute validates one model route and returns its compiled form. The
// returned error carries no route key; Compile wraps it.
func compileRoute(mr *model.Route, pickers map[model.BackendKey]upstream.Picker) (*Route, error) {
	if err := validateHost(mr.Host); err != nil {
		return nil, fmt.Errorf("host %q: %w", mr.Host, err)
	}
	path, err := normalizePath(mr.Path, mr.PathType)
	if err != nil {
		return nil, fmt.Errorf("path %q: %w", mr.Path, err)
	}
	picker, ok := pickers[mr.Backend]
	if !ok {
		return nil, fmt.Errorf("backend %s: no such backend in snapshot", mr.Backend)
	}
	req, err := compileHeaders(mr.RequestHeaders)
	if err != nil {
		return nil, fmt.Errorf("request headers: %w", err)
	}
	resp, err := compileHeaders(mr.ResponseHeaders)
	if err != nil {
		return nil, fmt.Errorf("response headers: %w", err)
	}
	return &Route{
		Key:             mr.Key,
		Host:            mr.Host,
		Path:            path,
		PathType:        mr.PathType,
		Namespace:       mr.Backend.Namespace,
		Service:         mr.Backend.Service,
		Backend:         picker,
		RequestHeaders:  req,
		ResponseHeaders: resp,
	}, nil
}

// normalizePath checks that path starts with "/" and, for a Prefix route,
// trims one trailing slash so "/foo/" and "/foo" are the same prefix. "/"
// stays "/". Exact paths are kept as given: their trailing slash is
// significant. The path type itself must be one of the two known values.
func normalizePath(path string, typ model.PathType) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", errors.New("path must start with \"/\"")
	}
	switch typ {
	case model.PathExact:
		return path, nil
	case model.PathPrefix:
		if trimmed := strings.TrimSuffix(path, "/"); trimmed != "" {
			return trimmed, nil
		}
		return "/", nil
	default:
		return "", fmt.Errorf("unknown path type %s", typ)
	}
}

// validateHost accepts "", a lowercase DNS name, or "*." followed by a
// lowercase DNS name of at least one label. The wildcard is allowed only as
// the whole leftmost label.
func validateHost(host string) error {
	if host == "" {
		return nil
	}
	name := host
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		if rest == "" {
			return errors.New("wildcard host needs at least one label after \"*.\"")
		}
		name = rest
	}
	if strings.Contains(name, "*") {
		return errors.New("wildcard is only allowed as the leftmost label")
	}
	if len(host) > maxHostLength {
		return fmt.Errorf("host is %d bytes, max %d", len(host), maxHostLength)
	}
	for _, label := range strings.Split(name, ".") {
		if err := validateLabel(label); err != nil {
			return err
		}
	}
	return nil
}

// validateLabel checks one DNS label: 1 to 63 bytes of lowercase letters,
// digits and hyphens, not starting or ending with a hyphen.
func validateLabel(label string) error {
	if label == "" {
		return errors.New("empty DNS label")
	}
	if len(label) > maxLabelLength {
		return fmt.Errorf("DNS label is %d bytes, max %d", len(label), maxLabelLength)
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case c == '-':
			if i == 0 || i == len(label)-1 {
				return errors.New("DNS label must not start or end with a hyphen")
			}
		case 'A' <= c && c <= 'Z':
			return errors.New("host must be lowercase")
		default:
			return fmt.Errorf("invalid byte %q in DNS label", c)
		}
	}
	return nil
}

// compileHeaders validates header rules structurally, checks each name and
// parses each value template. Error messages carry the rule index and the
// header name, never the value, which may be a secret.
func compileHeaders(rules []model.HeaderRule) ([]Header, error) {
	if err := model.ValidateHeaderRules(rules); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]Header, 0, len(rules))
	for i, rule := range rules {
		if err := placeholders.ValidateHeaderName(rule.Name); err != nil {
			return nil, fmt.Errorf("rule %d: header %q: %w", i, rule.Name, err)
		}
		tpl, err := placeholders.Parse(rule.Value)
		if err != nil {
			return nil, fmt.Errorf("rule %d: header %q: %w", i, rule.Name, err)
		}
		out = append(out, Header{Name: rule.Name, Value: tpl})
	}
	return out, nil
}

// builder accumulates compiled routes into host buckets and resolves
// conflicts between routes sharing (host, path, type).
type builder struct {
	routes   []*Route
	exact    map[string]*bucket
	wildcard map[string]*bucket
	catchAll *bucket
	// seen detects the same (host, path, type) under the same key, which is
	// an error rather than a conflict to resolve.
	seen map[routeIdentity]struct{}
}

// routeIdentity is the full identity of a compiled route.
type routeIdentity struct {
	host, path string
	typ        model.PathType
	key        model.RouteKey
}

// candidate is a compiled route with the creation time needed to settle
// conflicts. The time is not part of Route because the data plane never
// needs it.
type candidate struct {
	route   *Route
	created time.Time
}

// bucket collects the candidates of one host by path, one winner per path
// and type.
type bucket struct {
	exact    map[string]candidate
	prefixes map[string]candidate
}

func newBuilder() *builder {
	return &builder{
		exact:    make(map[string]*bucket),
		wildcard: make(map[string]*bucket),
		seen:     make(map[routeIdentity]struct{}),
	}
}

func newBucket() *bucket {
	return &bucket{exact: make(map[string]candidate), prefixes: make(map[string]candidate)}
}

// add records r. It rejects a route whose (host, path, type, key) was
// already seen and keeps the older candidate when only the key differs.
func (b *builder) add(r *Route, created time.Time) error {
	id := routeIdentity{host: r.Host, path: r.Path, typ: r.PathType, key: r.Key}
	if _, dup := b.seen[id]; dup {
		return fmt.Errorf("duplicate route: host %q path %q type %s", r.Host, r.Path, r.PathType)
	}
	b.seen[id] = struct{}{}
	b.routes = append(b.routes, r)

	var bk *bucket
	switch {
	case r.Host == "":
		if b.catchAll == nil {
			b.catchAll = newBucket()
		}
		bk = b.catchAll
	case strings.HasPrefix(r.Host, "*."):
		bk = getOrCreate(b.wildcard, r.Host)
	default:
		bk = getOrCreate(b.exact, r.Host)
	}

	byPath := bk.prefixes
	if r.PathType == model.PathExact {
		byPath = bk.exact
	}
	c := candidate{route: r, created: created}
	if cur, ok := byPath[r.Path]; !ok || wins(c, cur) {
		byPath[r.Path] = c
	}
	return nil
}

func getOrCreate(m map[string]*bucket, host string) *bucket {
	bk, ok := m[host]
	if !ok {
		bk = newBucket()
		m[host] = bk
	}
	return bk
}

// wins reports whether a beats b for the same (host, path, type): the oldest
// creation time wins, then the lexically smallest key.
func wins(a, b candidate) bool {
	if !a.created.Equal(b.created) {
		return a.created.Before(b.created)
	}
	return compareKeys(a.route.Key, b.route.Key) < 0
}

// compareKeys orders route keys by Namespace, Name, Rule and Path.
func compareKeys(a, b model.RouteKey) int {
	return cmp.Or(
		cmp.Compare(a.Namespace, b.Namespace),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.Rule, b.Rule),
		cmp.Compare(a.Path, b.Path),
	)
}

// table freezes the builder into an immutable Table.
func (b *builder) table() *Table {
	t := &Table{
		BuiltAt:  time.Now(),
		routes:   b.routes,
		exact:    make(map[string]*hostRoutes, len(b.exact)),
		wildcard: make(map[string]*hostRoutes, len(b.wildcard)),
	}
	for host, bk := range b.exact {
		t.exact[host] = bk.freeze()
	}
	for host, bk := range b.wildcard {
		t.wildcard[host] = bk.freeze()
	}
	if b.catchAll != nil {
		t.catchAll = b.catchAll.freeze()
	}
	return t
}

// freeze turns the winners of a bucket into the lookup structures Match
// uses: an exact-path map and a prefix slice sorted by path length
// descending, ties by creation time then key.
func (bk *bucket) freeze() *hostRoutes {
	hr := &hostRoutes{exact: make(map[string]*Route, len(bk.exact))}
	for path, c := range bk.exact {
		hr.exact[path] = c.route
	}
	prefixes := make([]candidate, 0, len(bk.prefixes))
	for _, c := range bk.prefixes {
		prefixes = append(prefixes, c)
	}
	slices.SortFunc(prefixes, func(a, b candidate) int {
		if d := cmp.Compare(len(b.route.Path), len(a.route.Path)); d != 0 {
			return d
		}
		if d := a.created.Compare(b.created); d != 0 {
			return d
		}
		return compareKeys(a.route.Key, b.route.Key)
	})
	hr.prefixes = make([]*Route, len(prefixes))
	for i, c := range prefixes {
		hr.prefixes[i] = c.route
	}
	return hr
}
