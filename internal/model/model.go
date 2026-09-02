// Package model defines the Kubernetes-independent intermediate routing model.
//
// The control plane translates Kubernetes objects into a Snapshot. The router
// validates and compiles a Snapshot into an immutable Table. No Kubernetes type
// appears in this package: it is pure data, and it is the only thing the
// control plane hands to the data plane.
package model

import (
	"fmt"
	"time"
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
// time and evaluated at request time.
type HeaderRule struct {
	Name  string
	Value string
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
func (s *Snapshot) Normalize() {
	// TODO(wave-2, model+router agent): implement.
}

// Hash returns a stable hash of the normalized snapshot, hex encoded. Callers
// must call Normalize first.
func (s *Snapshot) Hash() string {
	// TODO(wave-2, model+router agent): implement (FNV-64a).
	return ""
}
