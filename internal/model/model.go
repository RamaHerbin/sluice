// Package model defines the Kubernetes-independent intermediate routing model.
//
// The control plane translates Kubernetes objects into a Snapshot. The router
// validates and compiles a Snapshot into an immutable Table. No Kubernetes type
// appears in this package: it is pure data, and it is the only thing the
// control plane hands to the data plane.
package model

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Limits on header rules, shared by the controller (annotations) and the
// router (validation) so the two never drift.
const (
	// MaxHeaderRules bounds the number of rules per direction on one route.
	MaxHeaderRules = 32
	// MaxHeaderNameLength bounds a header name in bytes.
	MaxHeaderNameLength = 256
)

// PathType selects how a Route's Path is matched against a request path.
type PathType uint8

const (
	// PathPrefix matches element-wise on "/" boundaries: "/foo" matches
	// "/foo", "/foo/" and "/foo/bar", but never "/foobar".
	PathPrefix PathType = iota
	// PathExact matches the whole path, case-sensitively, trailing slash
	// included.
	PathExact
)

// String returns the Ingress-style name of the path type.
func (p PathType) String() string {
	switch p {
	case PathPrefix:
		return "Prefix"
	case PathExact:
		return "Exact"
	default:
		return fmt.Sprintf("PathType(%d)", uint8(p))
	}
}

// RouteKey identifies the Kubernetes rule a Route was derived from.
type RouteKey struct {
	Namespace string
	Name      string
	Rule      int // index in spec.rules
	Path      int // index in spec.rules[Rule].http.paths
}

// String renders the key as "namespace/name#r<rule>p<path>".
func (k RouteKey) String() string {
	return fmt.Sprintf("%s/%s#r%dp%d", k.Namespace, k.Name, k.Rule, k.Path)
}

// BackendKey identifies one port of one Service.
type BackendKey struct {
	Namespace string
	Service   string
	Port      int32
}

// String renders the key as "namespace/service:port".
func (k BackendKey) String() string {
	return fmt.Sprintf("%s/%s:%d", k.Namespace, k.Service, k.Port)
}

// Endpoint is one address a backend can be reached at.
type Endpoint struct {
	// Address is "host:port", as accepted by net.Dial.
	Address string
	// Ready reports whether the endpoint should receive traffic.
	Ready bool
}

// Backend groups the endpoints of one Service port.
type Backend struct {
	Key       BackendKey
	Endpoints []Endpoint
}

// HeaderRule sets a header on the upstream request or on the client response.
// Value is a raw placeholder template; it is parsed and validated at compile
// time and evaluated at request time. Set semantics only: no append, no
// delete.
type HeaderRule struct {
	Name  string
	Value string
}

// ParseHeaderRules parses the annotation format shared by
// sluice.dev/request-headers and sluice.dev/response-headers: one
// "Name: value" per line, split at the first colon, both sides trimmed, blank
// lines skipped, no comment syntax. It rejects a line without a colon, an
// empty name, an empty value, duplicate names (case-insensitive), more than
// MaxHeaderRules rules and names longer than MaxHeaderNameLength. It does not
// validate the name as a token nor parse the value as a template; that is
// the router's job through the placeholders package.
func ParseHeaderRules(text string) ([]HeaderRule, error) {
	var rules []HeaderRule
	// lineOf[i] is the 1-based line the i-th accepted rule came from, so a
	// duplicate can point at the earlier occurrence.
	var lineOf []int
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		// TrimSpace also removes the trailing "\r" of a CRLF line.
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		left, right, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("line %d: missing ':' between header name and value", lineNo)
		}
		rule := HeaderRule{Name: strings.TrimSpace(left), Value: strings.TrimSpace(right)}
		if err := checkRule(rule); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if j := indexFold(rules, rule.Name); j >= 0 {
			return nil, fmt.Errorf("line %d: header %q: duplicate of line %d", lineNo, rule.Name, lineOf[j])
		}
		if len(rules) == MaxHeaderRules {
			return nil, fmt.Errorf("line %d: more than %d header rules", lineNo, MaxHeaderRules)
		}
		rules = append(rules, rule)
		lineOf = append(lineOf, lineNo)
	}
	return rules, nil
}

// ValidateHeaderRules applies the structural checks of ParseHeaderRules to
// rules built by other means: non-empty names and values, no duplicate names
// (case-insensitive), at most MaxHeaderRules entries, names within
// MaxHeaderNameLength. Compile calls it so snapshots not produced by the
// controller are held to the same rules.
//
// Positions in error messages are zero-based indices into rules.
func ValidateHeaderRules(rules []HeaderRule) error {
	if len(rules) > MaxHeaderRules {
		return fmt.Errorf("%d header rules, max %d", len(rules), MaxHeaderRules)
	}
	for i, rule := range rules {
		if err := checkRule(rule); err != nil {
			return fmt.Errorf("rule %d: %w", i, err)
		}
		if j := indexFold(rules[:i], rule.Name); j >= 0 {
			return fmt.Errorf("rule %d: header %q: duplicate of rule %d", i, rule.Name, j)
		}
	}
	return nil
}

// checkRule holds the per-rule structural checks shared by ParseHeaderRules
// and ValidateHeaderRules. Messages carry the header name when it is short
// enough to be a real name, never the value: a static value can be a secret.
func checkRule(rule HeaderRule) error {
	if rule.Name == "" {
		return errors.New("empty header name")
	}
	if len(rule.Name) > MaxHeaderNameLength {
		return fmt.Errorf("header name is %d bytes, max %d", len(rule.Name), MaxHeaderNameLength)
	}
	if strings.TrimSpace(rule.Value) == "" {
		return fmt.Errorf("header %q: empty value", rule.Name)
	}
	return nil
}

// indexFold returns the index of the first rule whose name equals name under
// strings.EqualFold, or -1. Linear scan: there are at most MaxHeaderRules.
func indexFold(rules []HeaderRule, name string) int {
	for i, r := range rules {
		if strings.EqualFold(r.Name, name) {
			return i
		}
	}
	return -1
}

// Route is one host/path rule pointing at a backend.
type Route struct {
	Key RouteKey
	// Host is "", an exact host such as "a.example.com", or a wildcard such
	// as "*.example.com" (leftmost label only).
	Host     string
	Path     string
	PathType PathType
	Backend  BackendKey
	// RequestHeaders are added to the request sent upstream.
	RequestHeaders []HeaderRule
	// ResponseHeaders are added to the response sent to the client.
	ResponseHeaders []HeaderRule
	// Created is the creation time of the source object, used as a
	// deterministic tie-break between conflicting routes.
	Created time.Time
}

// Snapshot is the complete desired routing state at one point in time.
type Snapshot struct {
	Routes   []Route
	Backends []Backend
}

// Normalize sorts routes, backends and endpoints deterministically so that two
// snapshots describing the same state are equal and hash identically.
//
// Routes are ordered by Host, Path, PathType, then Key; backends by Key;
// endpoints by Address then Ready. Header rules keep their order because it
// is meaningful: the proxy applies them in annotation order. Normalize is
// idempotent and does not change which routes or backends exist.
func (s *Snapshot) Normalize() {
	if s == nil {
		return
	}
	slices.SortFunc(s.Routes, compareRoutes)
	slices.SortFunc(s.Backends, func(a, b Backend) int {
		return compareBackendKeys(a.Key, b.Key)
	})
	for i := range s.Backends {
		slices.SortFunc(s.Backends[i].Endpoints, func(a, b Endpoint) int {
			return cmp.Or(
				cmp.Compare(a.Address, b.Address),
				compareBools(a.Ready, b.Ready),
			)
		})
	}
}

// compareRoutes orders by Host, Path, PathType and Key. Backend and Created
// are tie-breaks only, so that two routes that share a Key (which Compile
// rejects anyway) still land in one canonical order.
func compareRoutes(a, b Route) int {
	return cmp.Or(
		cmp.Compare(a.Host, b.Host),
		cmp.Compare(a.Path, b.Path),
		cmp.Compare(a.PathType, b.PathType),
		cmp.Compare(a.Key.Namespace, b.Key.Namespace),
		cmp.Compare(a.Key.Name, b.Key.Name),
		cmp.Compare(a.Key.Rule, b.Key.Rule),
		cmp.Compare(a.Key.Path, b.Key.Path),
		compareBackendKeys(a.Backend, b.Backend),
		a.Created.Compare(b.Created),
	)
}

func compareBackendKeys(a, b BackendKey) int {
	return cmp.Or(
		cmp.Compare(a.Namespace, b.Namespace),
		cmp.Compare(a.Service, b.Service),
		cmp.Compare(a.Port, b.Port),
	)
}

// compareBools orders false before true.
func compareBools(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}

// Hash returns a stable hash of the normalized snapshot, hex encoded. Callers
// must call Normalize first. Route.Created is part of the hash on purpose (it
// drives the conflict tie-break), so deleting and recreating an Ingress with
// an identical spec yields a new hash and one rebuild.
//
// The hash is FNV-64a over the canonical serialization described in hash.go:
// every field of every route, backend and endpoint is written with a length
// prefix or a fixed width, so two snapshots that differ in any field produce
// different byte streams. Because slice order is part of the stream, a
// snapshot that was not normalized hashes differently from its normalized
// twin; that is why Normalize comes first.
func (s *Snapshot) Hash() string {
	var w canonicalWriter
	if s == nil {
		w.writeSnapshot(&Snapshot{})
	} else {
		w.writeSnapshot(s)
	}
	return w.sum()
}
