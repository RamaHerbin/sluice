package router

import (
	"errors"
	"testing"

	"github.com/RamaHerbin/sluice/internal/model"
)

// Backend keys shared by the tests.
var (
	beA = model.BackendKey{Namespace: "default", Service: "svc-a", Port: 80}
	beB = model.BackendKey{Namespace: "default", Service: "svc-b", Port: 80}
)

// readyBackend returns a Backend whose endpoints are all ready.
func readyBackend(key model.BackendKey, addrs ...string) model.Backend {
	b := model.Backend{Key: key}
	for _, a := range addrs {
		b.Endpoints = append(b.Endpoints, model.Endpoint{Address: a, Ready: true})
	}
	return b
}

// keyOf is the route key newRoute assigns to name.
func keyOf(name string) model.RouteKey {
	return model.RouteKey{Namespace: "default", Name: name}
}

// newRoute builds a model.Route keyed "default/<name>#r0p0". Tests identify
// the route Match returned through Route.Key.Name.
func newRoute(name, host, path string, typ model.PathType, be model.BackendKey) model.Route {
	return model.Route{
		Key:      keyOf(name),
		Host:     host,
		Path:     path,
		PathType: typ,
		Backend:  be,
	}
}

// mustCompile compiles s or fails the test.
func mustCompile(t *testing.T, s *model.Snapshot) *Table {
	t.Helper()
	tbl, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return tbl
}

// wantRouteError asserts that err wraps a *RouteError naming key.
func wantRouteError(t *testing.T, err error, key model.RouteKey) *RouteError {
	t.Helper()
	if err == nil {
		t.Fatal("Compile accepted an invalid snapshot")
	}
	var re *RouteError
	if !errors.As(err, &re) {
		t.Fatalf("error %T %v does not wrap *RouteError", err, err)
	}
	if re.Key != key {
		t.Fatalf("RouteError.Key = %v, want %v (error: %v)", re.Key, key, err)
	}
	return re
}
