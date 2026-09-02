package controller

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RamaHerbin/sluice/internal/model"
)

// created is the fixed creation timestamp of every test Ingress.
var created = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// Test object builders. They set explicitly what the API server would
// default, because neither Translate nor the fake clientset defaults anything.

func newIngressClass(name, controller string) *networkingv1.IngressClass {
	return &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       networkingv1.IngressClassSpec{Controller: controller},
	}
}

// newIngress builds an Ingress that names the "sluice" class.
func newIngress(ns, name string, rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       networkingv1.IngressSpec{IngressClassName: ptr(DefaultIngressClass), Rules: rules},
	}
}

func withAnnotations(ing *networkingv1.Ingress, kv ...string) *networkingv1.Ingress {
	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		ing.Annotations[kv[i]] = kv[i+1]
	}
	return ing
}

func httpRule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{
		Host:             host,
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}},
	}
}

func servicePath(path string, pt networkingv1.PathType, svc string, port networkingv1.ServiceBackendPort) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{
		Path:     path,
		PathType: ptr(pt),
		Backend: networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: svc, Port: port},
		},
	}
}

func byNumber(n int32) networkingv1.ServiceBackendPort {
	return networkingv1.ServiceBackendPort{Number: n}
}
func byName(n string) networkingv1.ServiceBackendPort {
	return networkingv1.ServiceBackendPort{Name: n}
}

func newService(ns, name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: ports},
	}
}

func tcpPort(name string, number int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: number, Protocol: corev1.ProtocolTCP}
}

// newSlice builds an EndpointSlice with one TCP port. A nil portName leaves
// the port unnamed, which is what a single unnamed Service port produces.
func newSlice(ns, name, svc string, at discoveryv1.AddressType, portName *string, portNumber int32, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svc},
		},
		AddressType: at,
		Ports:       []discoveryv1.EndpointPort{{Name: portName, Port: ptr(portNumber), Protocol: ptr(corev1.ProtocolTCP)}},
		Endpoints:   eps,
	}
}

func endpoint(addr string, ready *bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{Addresses: []string{addr}, Conditions: discoveryv1.EndpointConditions{Ready: ready}}
}

// Common fixture: namespace demo, Service echo with port http:80 and one
// slice with two ready endpoints on 8080.
const (
	ns  = "demo"
	svc = "echo"
)

func echoService() *corev1.Service { return newService(ns, svc, tcpPort("http", 80)) }

func echoSlice() *discoveryv1.EndpointSlice {
	return newSlice(ns, "echo-abc", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080,
		endpoint("10.0.0.1", ptr(true)),
		endpoint("10.0.0.2", ptr(true)),
	)
}

func echoBackend() model.Backend {
	return model.Backend{
		Key: model.BackendKey{Namespace: ns, Service: svc, Port: 80},
		Endpoints: []model.Endpoint{
			{Address: "10.0.0.1:8080", Ready: true},
			{Address: "10.0.0.2:8080", Ready: true},
		},
	}
}

func baseInput(ings ...*networkingv1.Ingress) Input {
	return Input{
		IngressClass:   DefaultIngressClass,
		ControllerName: DefaultControllerName,
		IngressClasses: []*networkingv1.IngressClass{newIngressClass(DefaultIngressClass, DefaultControllerName)},
		Ingresses:      ings,
		Services:       []*corev1.Service{echoService()},
		Slices:         []*discoveryv1.EndpointSlice{echoSlice()},
	}
}

func routeKey(name string, rule, path int) model.RouteKey {
	return model.RouteKey{Namespace: ns, Name: name, Rule: rule, Path: path}
}

func routeKeys(snap *model.Snapshot) []string {
	keys := make([]string, 0, len(snap.Routes))
	for _, r := range snap.Routes {
		keys = append(keys, r.Key.String())
	}
	return keys
}

func assertSnapshot(t *testing.T, got, want *model.Snapshot) {
	t.Helper()
	want.Normalize()
	if !reflect.DeepEqual(got.Routes, want.Routes) {
		t.Errorf("routes\n got: %+v\nwant: %+v", got.Routes, want.Routes)
	}
	if !reflect.DeepEqual(got.Backends, want.Backends) {
		t.Errorf("backends\n got: %+v\nwant: %+v", got.Backends, want.Backends)
	}
}

func assertNoWarnings(t *testing.T, warnings []Warning) {
	t.Helper()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
}

func TestTranslateClaiming(t *testing.T) {
	otherClass := newIngressClass("other", "example.com/other-controller")
	rule := httpRule("echo.example.com", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))

	tests := []struct {
		name    string
		ingress func() *networkingv1.Ingress
		claimed bool
	}{
		{
			name:    "class whose controller is ours",
			ingress: func() *networkingv1.Ingress { return newIngress(ns, "web", rule) },
			claimed: true,
		},
		{
			name: "class owned by another controller",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = ptr("other")
				return ing
			},
		},
		{
			name: "class name that does not exist",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = ptr("missing")
				return ing
			},
		},
		{
			name: "legacy annotation equal to the class name",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = nil
				return withAnnotations(ing, LegacyClassAnnotation, DefaultIngressClass)
			},
			claimed: true,
		},
		{
			name: "legacy annotation with another class name",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = nil
				return withAnnotations(ing, LegacyClassAnnotation, "nginx")
			},
		},
		{
			name: "legacy annotation ignored when a class name is set",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = ptr("other")
				return withAnnotations(ing, LegacyClassAnnotation, DefaultIngressClass)
			},
		},
		{
			name: "no class and no annotation",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = nil
				return ing
			},
		},
		{
			name: "empty class name falls back to the annotation",
			ingress: func() *networkingv1.Ingress {
				ing := newIngress(ns, "web", rule)
				ing.Spec.IngressClassName = ptr("")
				return withAnnotations(ing, LegacyClassAnnotation, DefaultIngressClass)
			},
			claimed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(tc.ingress())
			in.IngressClasses = append(in.IngressClasses, otherClass)
			snap, warnings := Translate(in)
			// Unclaimed Ingresses are ignored silently, never warned about.
			assertNoWarnings(t, warnings)
			if got := len(snap.Routes) == 1; got != tc.claimed {
				t.Fatalf("claimed = %v, want %v (routes %v)", got, tc.claimed, routeKeys(snap))
			}
			if tc.claimed && len(snap.Backends) != 1 {
				t.Fatalf("backends = %+v, want one", snap.Backends)
			}
		})
	}
}

func TestTranslateDefaultsClassAndController(t *testing.T) {
	in := baseInput(newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))))
	in.IngressClass = ""
	in.ControllerName = ""
	snap, warnings := Translate(in)
	assertNoWarnings(t, warnings)
	if len(snap.Routes) != 1 {
		t.Fatalf("routes = %v, want one", routeKeys(snap))
	}
}

func TestTranslatePathsHostsAndKeys(t *testing.T) {
	ing := newIngress(ns, "web",
		httpRule("Echo.Example.COM",
			servicePath("/exact", networkingv1.PathTypeExact, svc, byNumber(80)),
			servicePath("/prefix", networkingv1.PathTypePrefix, svc, byNumber(80)),
			servicePath("/impl", networkingv1.PathTypeImplementationSpecific, svc, byNumber(80)),
		),
		httpRule("*.Example.com",
			servicePath("", networkingv1.PathTypeImplementationSpecific, svc, byNumber(80)),
		),
		// A rule without an http block is skipped without a warning.
		networkingv1.IngressRule{Host: "nohttp.example.com"},
		httpRule("",
			servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)),
		),
	)
	snap, warnings := Translate(baseInput(ing))
	assertNoWarnings(t, warnings)

	backend := model.BackendKey{Namespace: ns, Service: svc, Port: 80}
	route := func(rule, path int, host, p string, pt model.PathType) model.Route {
		return model.Route{Key: routeKey("web", rule, path), Host: host, Path: p, PathType: pt, Backend: backend, Created: created}
	}
	assertSnapshot(t, snap, &model.Snapshot{
		Routes: []model.Route{
			route(0, 0, "echo.example.com", "/exact", model.PathExact),
			route(0, 1, "echo.example.com", "/prefix", model.PathPrefix),
			route(0, 2, "echo.example.com", "/impl", model.PathPrefix),
			route(1, 0, "*.example.com", "/", model.PathPrefix),
			route(3, 0, "", "/", model.PathPrefix),
		},
		Backends: []model.Backend{echoBackend()},
	})
}

func TestTranslateNilPathTypeIsPrefix(t *testing.T) {
	p := servicePath("/x", networkingv1.PathTypeExact, svc, byNumber(80))
	p.PathType = nil
	snap, warnings := Translate(baseInput(newIngress(ns, "web", httpRule("", p))))
	assertNoWarnings(t, warnings)
	if len(snap.Routes) != 1 || snap.Routes[0].PathType != model.PathPrefix {
		t.Fatalf("routes = %+v, want one Prefix route", snap.Routes)
	}
}

func TestTranslateDefaultBackendIgnored(t *testing.T) {
	ing := newIngress(ns, "web")
	ing.Spec.DefaultBackend = &networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{Name: svc, Port: byNumber(80)},
	}
	snap, warnings := Translate(baseInput(ing))
	assertNoWarnings(t, warnings)
	if len(snap.Routes) != 0 || len(snap.Backends) != 0 {
		t.Fatalf("snapshot = %+v, want empty", snap)
	}
}

func TestTranslateHeaderAnnotations(t *testing.T) {
	rule := httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))

	tests := []struct {
		name        string
		annotation  string
		value       string
		wantRules   []model.HeaderRule // request or response rules, per annotation
		wantSkipped bool
		wantReason  []string // substrings the reason must contain
		leak        string   // a substring the reason must not contain
	}{
		{
			name:       "request rules with a blank line",
			annotation: AnnotationRequestHeaders,
			value:      "X-A: 1\n\nX-B: {http.request.host}\n",
			wantRules:  []model.HeaderRule{{Name: "X-A", Value: "1"}, {Name: "X-B", Value: "{http.request.host}"}},
		},
		{
			name:       "response rules keep annotation order",
			annotation: AnnotationResponseHeaders,
			value:      "X-Served-By: {k8s.namespace}/{k8s.service}\nX-Route: {k8s.route}",
			wantRules:  []model.HeaderRule{{Name: "X-Served-By", Value: "{k8s.namespace}/{k8s.service}"}, {Name: "X-Route", Value: "{k8s.route}"}},
		},
		{
			name:       "value with a colon",
			annotation: AnnotationRequestHeaders,
			value:      "X-A: a:b",
			wantRules:  []model.HeaderRule{{Name: "X-A", Value: "a:b"}},
		},
		{
			name:       "whitespace and CRLF trimmed",
			annotation: AnnotationRequestHeaders,
			value:      "\tX-A :  v  \r\n",
			wantRules:  []model.HeaderRule{{Name: "X-A", Value: "v"}},
		},
		{
			name:        "duplicate name case-insensitive",
			annotation:  AnnotationRequestHeaders,
			value:       "X-A: 1\nx-a: 2",
			wantSkipped: true,
			wantReason:  []string{AnnotationRequestHeaders, "line 2", "duplicate"},
		},
		{
			name:        "missing colon",
			annotation:  AnnotationRequestHeaders,
			value:       "X-A",
			wantSkipped: true,
			wantReason:  []string{"line 1", "missing ':'"},
		},
		{
			name:        "empty value",
			annotation:  AnnotationRequestHeaders,
			value:       "X-A:",
			wantSkipped: true,
			wantReason:  []string{"line 1", "empty value"},
		},
		{
			name:        "unknown placeholder on line 3 (Scenario 12)",
			annotation:  AnnotationRequestHeaders,
			value:       "X-A: 1\n\nX-B: topsecret {nope}",
			wantSkipped: true,
			wantReason:  []string{AnnotationRequestHeaders, "line 3", `"X-B"`, "unknown placeholder {nope}"},
			leak:        "topsecret",
		},
		{
			name:        "unbalanced braces in a response rule",
			annotation:  AnnotationResponseHeaders,
			value:       "X-A: {http.request.host",
			wantSkipped: true,
			wantReason:  []string{AnnotationResponseHeaders, "line 1", "unbalanced"},
		},
		{
			name:        "denied header name",
			annotation:  AnnotationRequestHeaders,
			value:       "Host: example.com",
			wantSkipped: true,
			wantReason:  []string{"line 1", `"Host"`, "reserved"},
			leak:        "example.com",
		},
		{
			name:        "header name is not a token",
			annotation:  AnnotationRequestHeaders,
			value:       "# comment: value",
			wantSkipped: true,
			wantReason:  []string{"line 1", "token"},
		},
		{
			name:        "more than 32 rules",
			annotation:  AnnotationRequestHeaders,
			value:       manyRules(33),
			wantSkipped: true,
			wantReason:  []string{"line 33", "more than 32"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := withAnnotations(newIngress(ns, "bad", rule), tc.annotation, tc.value)
			good := newIngress(ns, "good", rule)
			snap, warnings := Translate(baseInput(bad, good))

			if !tc.wantSkipped {
				assertNoWarnings(t, warnings)
				if len(snap.Routes) != 2 {
					t.Fatalf("routes = %v, want two", routeKeys(snap))
				}
				var got []model.HeaderRule
				for _, r := range snap.Routes {
					if r.Key.Name != "bad" {
						continue
					}
					if tc.annotation == AnnotationRequestHeaders {
						got = r.RequestHeaders
					} else {
						got = r.ResponseHeaders
					}
				}
				if !reflect.DeepEqual(got, tc.wantRules) {
					t.Fatalf("rules = %+v, want %+v", got, tc.wantRules)
				}
				return
			}

			// The bad Ingress is skipped with one warning and the other
			// Ingress is still translated.
			if len(warnings) != 1 {
				t.Fatalf("warnings = %+v, want exactly one", warnings)
			}
			w := warnings[0]
			if w.Object != ns+"/bad" {
				t.Errorf("warning object = %q, want %q", w.Object, ns+"/bad")
			}
			for _, sub := range tc.wantReason {
				if !strings.Contains(w.Reason, sub) {
					t.Errorf("reason %q does not contain %q", w.Reason, sub)
				}
			}
			if tc.leak != "" && strings.Contains(w.Reason, tc.leak) {
				t.Errorf("reason %q leaks the annotation value %q", w.Reason, tc.leak)
			}
			if keys := routeKeys(snap); len(keys) != 1 || keys[0] != "demo/good#r0p0" {
				t.Fatalf("routes = %v, want only demo/good#r0p0", keys)
			}
		})
	}
}

// TestScenario12UnknownPlaceholderSkipsOnlyThatIngress is the controller-level
// half of scenario 12: an unknown placeholder skips its Ingress with a
// Warning and leaves the others untouched.
func TestScenario12UnknownPlaceholderSkipsOnlyThatIngress(t *testing.T) {
	rule := httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))
	bad := withAnnotations(newIngress(ns, "bad", rule), AnnotationRequestHeaders, "X-Route: {k8s.rouet}")
	good := withAnnotations(newIngress(ns, "good", rule), AnnotationRequestHeaders, "X-Route: {k8s.route}")

	snap, warnings := Translate(baseInput(bad, good))
	if len(warnings) != 1 || warnings[0].Object != "demo/bad" || !strings.Contains(warnings[0].Reason, "unknown placeholder {k8s.rouet}") {
		t.Fatalf("warnings = %+v", warnings)
	}
	if keys := routeKeys(snap); len(keys) != 1 || keys[0] != "demo/good#r0p0" {
		t.Fatalf("routes = %v, want only demo/good#r0p0", keys)
	}
	if got := snap.Routes[0].RequestHeaders; !reflect.DeepEqual(got, []model.HeaderRule{{Name: "X-Route", Value: "{k8s.route}"}}) {
		t.Fatalf("request headers = %+v", got)
	}
}

func manyRules(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("X-")
		b.WriteString(strings.Repeat("a", i+1))
		b.WriteString(": v\n")
	}
	return b.String()
}

func TestTranslateSkipsIngressWithResourceBackend(t *testing.T) {
	resource := networkingv1.HTTPIngressPath{
		Path:     "/static",
		PathType: ptr(networkingv1.PathTypePrefix),
		Backend: networkingv1.IngressBackend{
			Resource: &corev1.TypedLocalObjectReference{APIGroup: ptr("storage.example.com"), Kind: "Bucket", Name: "assets"},
		},
	}
	bad := newIngress(ns, "bad",
		httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))),
		httpRule("", resource),
	)
	good := newIngress(ns, "good", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))

	snap, warnings := Translate(baseInput(bad, good))
	if len(warnings) != 1 || warnings[0].Object != "demo/bad" {
		t.Fatalf("warnings = %+v, want one for demo/bad", warnings)
	}
	for _, sub := range []string{"rule 1 path 0", "resource backend"} {
		if !strings.Contains(warnings[0].Reason, sub) {
			t.Errorf("reason %q does not contain %q", warnings[0].Reason, sub)
		}
	}
	// The whole Ingress is skipped, including its valid first rule.
	if keys := routeKeys(snap); len(keys) != 1 || keys[0] != "demo/good#r0p0" {
		t.Fatalf("routes = %v, want only demo/good#r0p0", keys)
	}
}

func TestTranslateSkipsIngressWithoutBackend(t *testing.T) {
	p := networkingv1.HTTPIngressPath{Path: "/", PathType: ptr(networkingv1.PathTypePrefix)}
	snap, warnings := Translate(baseInput(newIngress(ns, "bad", httpRule("", p))))
	if len(warnings) != 1 || !strings.Contains(warnings[0].Reason, "neither a service nor a resource") {
		t.Fatalf("warnings = %+v", warnings)
	}
	if len(snap.Routes) != 0 {
		t.Fatalf("routes = %v, want none", routeKeys(snap))
	}
}

func TestTranslateSkipsIngressWithExternalNameService(t *testing.T) {
	external := newService(ns, "ext")
	external.Spec.Type = corev1.ServiceTypeExternalName
	external.Spec.ExternalName = "api.example.com"

	bad := newIngress(ns, "bad", httpRule("", servicePath("/", networkingv1.PathTypePrefix, "ext", byNumber(80))))
	good := newIngress(ns, "good", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
	in := baseInput(bad, good)
	in.Services = append(in.Services, external)

	snap, warnings := Translate(in)
	if len(warnings) != 1 || warnings[0].Object != "demo/bad" {
		t.Fatalf("warnings = %+v, want one for demo/bad", warnings)
	}
	for _, sub := range []string{"rule 0 path 0", "demo/ext", "ExternalName"} {
		if !strings.Contains(warnings[0].Reason, sub) {
			t.Errorf("reason %q does not contain %q", warnings[0].Reason, sub)
		}
	}
	if keys := routeKeys(snap); len(keys) != 1 || keys[0] != "demo/good#r0p0" {
		t.Fatalf("routes = %v, want only demo/good#r0p0", keys)
	}
}

func TestTranslateMissingService(t *testing.T) {
	tests := []struct {
		name     string
		port     networkingv1.ServiceBackendPort
		wantPort int32
	}{
		{name: "by number keeps the number", port: byNumber(8080), wantPort: 8080},
		{name: "by name cannot be resolved and uses 0", port: byName("http"), wantPort: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ing := newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, "ghost", tc.port)))
			snap, warnings := Translate(baseInput(ing))
			// Not a warning: the route is kept and answers 503 until the
			// Service appears.
			assertNoWarnings(t, warnings)
			key := model.BackendKey{Namespace: ns, Service: "ghost", Port: tc.wantPort}
			assertSnapshot(t, snap, &model.Snapshot{
				Routes: []model.Route{{
					Key: routeKey("web", 0, 0), Host: "", Path: "/", PathType: model.PathPrefix, Backend: key, Created: created,
				}},
				Backends: []model.Backend{{Key: key}},
			})
		})
	}
}

func TestTranslatePortNotOnService(t *testing.T) {
	ing := newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byName("grpc"))))
	snap, warnings := Translate(baseInput(ing))
	assertNoWarnings(t, warnings)
	key := model.BackendKey{Namespace: ns, Service: svc, Port: 0}
	if len(snap.Routes) != 1 || snap.Routes[0].Backend != key {
		t.Fatalf("routes = %+v, want one route to %s", snap.Routes, key)
	}
	if len(snap.Backends) != 1 || snap.Backends[0].Key != key || len(snap.Backends[0].Endpoints) != 0 {
		t.Fatalf("backends = %+v, want %s with no endpoints", snap.Backends, key)
	}
}

func TestTranslatePortByNameAndByNumber(t *testing.T) {
	service := newService(ns, svc, tcpPort("http", 80), tcpPort("metrics", 9090))
	slice := newSlice(ns, "echo-abc", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080, endpoint("10.0.0.1", ptr(true)))
	slice.Ports = append(slice.Ports, discoveryv1.EndpointPort{Name: ptr("metrics"), Port: ptr[int32](9091), Protocol: ptr(corev1.ProtocolTCP)})

	ing := newIngress(ns, "web", httpRule("",
		servicePath("/by-name", networkingv1.PathTypePrefix, svc, byName("http")),
		servicePath("/by-number", networkingv1.PathTypePrefix, svc, byNumber(80)),
		servicePath("/metrics", networkingv1.PathTypePrefix, svc, byNumber(9090)),
	))
	in := baseInput(ing)
	in.Services = []*corev1.Service{service}
	in.Slices = []*discoveryv1.EndpointSlice{slice}

	snap, warnings := Translate(in)
	assertNoWarnings(t, warnings)

	http := model.BackendKey{Namespace: ns, Service: svc, Port: 80}
	metrics := model.BackendKey{Namespace: ns, Service: svc, Port: 9090}
	route := func(path int, p string, key model.BackendKey) model.Route {
		return model.Route{Key: routeKey("web", 0, path), Path: p, PathType: model.PathPrefix, Backend: key, Created: created}
	}
	assertSnapshot(t, snap, &model.Snapshot{
		Routes: []model.Route{
			route(0, "/by-name", http),
			route(1, "/by-number", http),
			route(2, "/metrics", metrics),
		},
		// Two routes share the http backend: one Backend, deduped by key.
		Backends: []model.Backend{
			{Key: http, Endpoints: []model.Endpoint{{Address: "10.0.0.1:8080", Ready: true}}},
			{Key: metrics, Endpoints: []model.Endpoint{{Address: "10.0.0.1:9091", Ready: true}}},
		},
	})
}

func TestTranslateSingleUnnamedPort(t *testing.T) {
	service := newService(ns, svc, tcpPort("", 80))
	slice := newSlice(ns, "echo-abc", svc, discoveryv1.AddressTypeIPv4, nil, 8080, endpoint("10.0.0.1", nil))
	ing := newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
	in := baseInput(ing)
	in.Services = []*corev1.Service{service}
	in.Slices = []*discoveryv1.EndpointSlice{slice}

	snap, warnings := Translate(in)
	assertNoWarnings(t, warnings)
	want := []model.Backend{{
		Key:       model.BackendKey{Namespace: ns, Service: svc, Port: 80},
		Endpoints: []model.Endpoint{{Address: "10.0.0.1:8080", Ready: true}},
	}}
	if !reflect.DeepEqual(snap.Backends, want) {
		t.Fatalf("backends = %+v, want %+v", snap.Backends, want)
	}
}

// TestScenario04RemovedEndpointAbsent is the translation half of scenario 4:
// readiness conditions are honoured and an endpoint that left the slice is
// not in the snapshot.
func TestScenario04RemovedEndpointAbsent(t *testing.T) {
	ing := newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
	slice := newSlice(ns, "echo-abc", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080,
		endpoint("10.0.0.1", ptr(true)),
		endpoint("10.0.0.2", nil), // nil means ready
		endpoint("10.0.0.3", ptr(false)),
		endpoint("10.0.0.4", ptr(true)),
	)
	in := baseInput(ing)
	in.Slices = []*discoveryv1.EndpointSlice{slice}

	snap, warnings := Translate(in)
	assertNoWarnings(t, warnings)
	want := []model.Endpoint{
		{Address: "10.0.0.1:8080", Ready: true},
		{Address: "10.0.0.2:8080", Ready: true},
		{Address: "10.0.0.3:8080", Ready: false},
		{Address: "10.0.0.4:8080", Ready: true},
	}
	if len(snap.Backends) != 1 || !reflect.DeepEqual(snap.Backends[0].Endpoints, want) {
		t.Fatalf("backends = %+v, want endpoints %+v", snap.Backends, want)
	}

	// The pod behind 10.0.0.4 goes away: the EndpointSlice controller drops
	// it from the slice, and the next translation no longer lists it.
	slice.Endpoints = slice.Endpoints[:3]
	snap, warnings = Translate(in)
	assertNoWarnings(t, warnings)
	if len(snap.Backends) != 1 || !reflect.DeepEqual(snap.Backends[0].Endpoints, want[:3]) {
		t.Fatalf("after removal backends = %+v, want endpoints %+v", snap.Backends, want[:3])
	}
}

func TestTranslateSliceFiltering(t *testing.T) {
	ing := newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))

	ipv6 := newSlice(ns, "echo-v6", svc, discoveryv1.AddressTypeIPv6, ptr("http"), 8080, endpoint("fd00::1", ptr(true)))
	fqdn := newSlice(ns, "echo-fqdn", svc, discoveryv1.AddressTypeFQDN, ptr("http"), 8080, endpoint("echo.internal", ptr(true)))
	udp := newSlice(ns, "echo-udp", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080, endpoint("10.0.0.9", ptr(true)))
	udp.Ports[0].Protocol = ptr(corev1.ProtocolUDP)
	otherPort := newSlice(ns, "echo-other", svc, discoveryv1.AddressTypeIPv4, ptr("metrics"), 9091, endpoint("10.0.0.8", ptr(true)))
	nilPort := newSlice(ns, "echo-nilport", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080, endpoint("10.0.0.7", ptr(true)))
	nilPort.Ports[0].Port = nil
	otherService := newSlice(ns, "other-abc", "other", discoveryv1.AddressTypeIPv4, ptr("http"), 8080, endpoint("10.0.0.6", ptr(true)))
	otherNamespace := newSlice("elsewhere", "echo-abc", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080, endpoint("10.0.0.5", ptr(true)))
	noAddress := newSlice(ns, "echo-empty", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080, discoveryv1.Endpoint{})
	unlabelled := newSlice(ns, "echo-nolabel", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080, endpoint("10.0.0.4", ptr(true)))
	unlabelled.Labels = nil

	in := baseInput(ing)
	in.Slices = []*discoveryv1.EndpointSlice{echoSlice(), ipv6, fqdn, udp, otherPort, nilPort, otherService, otherNamespace, noAddress, unlabelled}

	snap, warnings := Translate(in)
	assertNoWarnings(t, warnings)
	want := []model.Endpoint{
		{Address: "10.0.0.1:8080", Ready: true},
		{Address: "10.0.0.2:8080", Ready: true},
		{Address: "[fd00::1]:8080", Ready: true},
	}
	if len(snap.Backends) != 1 || !reflect.DeepEqual(snap.Backends[0].Endpoints, want) {
		t.Fatalf("backends = %+v, want endpoints %+v", snap.Backends, want)
	}
}

func TestTranslateMultipleSlicesMerged(t *testing.T) {
	ing := newIngress(ns, "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
	a := newSlice(ns, "echo-a", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080,
		endpoint("10.0.0.1", ptr(true)),
		endpoint("10.0.0.2", ptr(false)),
	)
	b := newSlice(ns, "echo-b", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080,
		endpoint("10.0.0.2", ptr(true)), // same address as in a, ready here
		endpoint("10.0.0.3", ptr(true)),
	)
	in := baseInput(ing)
	in.Slices = []*discoveryv1.EndpointSlice{a, b}

	snap, warnings := Translate(in)
	assertNoWarnings(t, warnings)
	want := []model.Endpoint{
		{Address: "10.0.0.1:8080", Ready: true},
		{Address: "10.0.0.2:8080", Ready: true},
		{Address: "10.0.0.3:8080", Ready: true},
	}
	if len(snap.Backends) != 1 || !reflect.DeepEqual(snap.Backends[0].Endpoints, want) {
		t.Fatalf("backends = %+v, want endpoints %+v", snap.Backends, want)
	}
}

func TestTranslateSharedBackendAcrossIngresses(t *testing.T) {
	a := newIngress(ns, "a", httpRule("a.example.com", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
	b := newIngress(ns, "b", httpRule("b.example.com", servicePath("/", networkingv1.PathTypePrefix, svc, byName("http"))))
	snap, warnings := Translate(baseInput(a, b))
	assertNoWarnings(t, warnings)
	if len(snap.Routes) != 2 {
		t.Fatalf("routes = %v, want two", routeKeys(snap))
	}
	if !reflect.DeepEqual(snap.Backends, []model.Backend{echoBackend()}) {
		t.Fatalf("backends = %+v, want exactly the shared echo backend", snap.Backends)
	}
}

func TestTranslateDeterministic(t *testing.T) {
	// Several Ingresses, two Services, several slices, one bad Ingress, all
	// shuffled between runs: the snapshot, its hash and the warnings must
	// not move.
	other := newService(ns, "other", tcpPort("http", 80))
	otherSlice := newSlice(ns, "other-abc", "other", discoveryv1.AddressTypeIPv4, ptr("http"), 9000,
		endpoint("10.1.0.1", ptr(true)), endpoint("10.1.0.2", ptr(true)))
	extra := newSlice(ns, "echo-def", svc, discoveryv1.AddressTypeIPv4, ptr("http"), 8080,
		endpoint("10.0.0.3", ptr(true)), endpoint("10.0.0.4", ptr(false)))
	ings := []*networkingv1.Ingress{
		newIngress(ns, "c", httpRule("c.example.com", servicePath("/", networkingv1.PathTypePrefix, "other", byNumber(80)))),
		newIngress(ns, "a", httpRule("a.example.com",
			servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)),
			servicePath("/api", networkingv1.PathTypeExact, "other", byName("http")),
		)),
		newIngress(ns, "b", httpRule("", servicePath("/b", networkingv1.PathTypePrefix, svc, byNumber(80)))),
		withAnnotations(newIngress(ns, "bad1", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))),
			AnnotationRequestHeaders, "X: {nope}"),
		withAnnotations(newIngress(ns, "bad2", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))),
			AnnotationResponseHeaders, "X-A"),
	}
	in := baseInput(ings...)
	in.Services = append(in.Services, other)
	in.Slices = append(in.Slices, otherSlice, extra)

	first, firstWarnings := Translate(in)
	if len(first.Routes) != 4 || len(first.Backends) != 2 || len(firstWarnings) != 2 {
		t.Fatalf("routes %d backends %d warnings %d", len(first.Routes), len(first.Backends), len(firstWarnings))
	}
	if firstWarnings[0].Object != "demo/bad1" || firstWarnings[1].Object != "demo/bad2" {
		t.Fatalf("warnings not in namespace/name order: %+v", firstWarnings)
	}

	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20; i++ {
		shuffled := in
		shuffled.Ingresses = shuffle(r, in.Ingresses)
		shuffled.Services = shuffle(r, in.Services)
		shuffled.Slices = shuffle(r, in.Slices)
		snap, warnings := Translate(shuffled)
		if snap.Hash() != first.Hash() {
			t.Fatalf("run %d: hash %s != %s", i, snap.Hash(), first.Hash())
		}
		if !reflect.DeepEqual(snap, first) {
			t.Fatalf("run %d: snapshot differs\n got: %+v\nwant: %+v", i, snap, first)
		}
		if !reflect.DeepEqual(warnings, firstWarnings) {
			t.Fatalf("run %d: warnings differ\n got: %+v\nwant: %+v", i, warnings, firstWarnings)
		}
	}
}

func shuffle[T any](r *rand.Rand, in []T) []T {
	out := make([]T, len(in))
	copy(out, in)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func TestTranslateDoesNotModifyInput(t *testing.T) {
	ings := []*networkingv1.Ingress{
		newIngress(ns, "b", httpRule("", servicePath("/b", networkingv1.PathTypePrefix, svc, byNumber(80)))),
		newIngress(ns, "a", httpRule("", servicePath("/a", networkingv1.PathTypePrefix, svc, byNumber(80)))),
	}
	in := baseInput(ings...)
	Translate(in)
	if ings[0].Name != "b" || ings[1].Name != "a" {
		t.Fatalf("input slice was reordered: %s, %s", ings[0].Name, ings[1].Name)
	}
}

func TestRuleLines(t *testing.T) {
	got := ruleLines("X-A: 1\n\n  \nX-B: 2\r\nX-C: 3")
	want := []int{1, 4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ruleLines = %v, want %v", got, want)
	}
	if lineOf(got, 7) != 0 {
		t.Fatal("lineOf out of range should be 0")
	}
}
