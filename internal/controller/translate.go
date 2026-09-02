package controller

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/placeholders"
)

// translator holds the indexed inputs of one Translate call plus the backends
// resolved so far. It is built and dropped inside Translate.
type translator struct {
	className string
	// classes holds the names of the IngressClasses whose controller is ours.
	classes map[string]bool
	// services is keyed by "namespace/name".
	services map[string]*corev1.Service
	// slices groups EndpointSlices by "namespace/serviceName", the same key
	// the informer index uses.
	slices map[string][]*discoveryv1.EndpointSlice
	// backends memoizes endpoint resolution per BackendKey so two routes to
	// the same Service port share one Backend.
	backends map[model.BackendKey]model.Backend
}

// backendRef is what a route needs to know about its backend: the key that
// goes into the Route, and enough to resolve endpoints once the Ingress is
// accepted. service is nil when the Service does not exist or has no matching
// port; such a backend has no endpoints and the proxy answers 503.
type backendRef struct {
	key      model.BackendKey
	service  *corev1.Service
	portName string // Service port name, matched against EndpointSlice ports
}

func newTranslator(in Input) *translator {
	className := in.IngressClass
	if className == "" {
		className = DefaultIngressClass
	}
	controllerName := in.ControllerName
	if controllerName == "" {
		controllerName = DefaultControllerName
	}

	t := &translator{
		className: className,
		classes:   make(map[string]bool, len(in.IngressClasses)),
		services:  make(map[string]*corev1.Service, len(in.Services)),
		slices:    make(map[string][]*discoveryv1.EndpointSlice),
		backends:  make(map[model.BackendKey]model.Backend),
	}
	for _, class := range in.IngressClasses {
		if class != nil && class.Spec.Controller == controllerName {
			t.classes[class.Name] = true
		}
	}
	for _, svc := range in.Services {
		if svc != nil {
			t.services[objectKey(svc)] = svc
		}
	}
	for _, slice := range in.Slices {
		if slice == nil {
			continue
		}
		if svc := slice.Labels[discoveryv1.LabelServiceName]; svc != "" {
			key := slice.Namespace + "/" + svc
			t.slices[key] = append(t.slices[key], slice)
		}
	}
	return t
}

// claims reports whether ing belongs to this controller: either
// spec.ingressClassName names an IngressClass with our controller, or the
// class name is empty and the deprecated annotation equals our class name.
// Anything else, including an unknown class name, is another controller's
// business and is ignored silently.
func (t *translator) claims(ing *networkingv1.Ingress) bool {
	if name := ing.Spec.IngressClassName; name != nil && *name != "" {
		return t.classes[*name]
	}
	return ing.Annotations[LegacyClassAnnotation] == t.className
}

// translateIngress returns the routes of one Ingress and the backends they
// reference, or an error saying why the whole Ingress must be skipped. The
// error message is the Warning reason: it names annotations, line numbers,
// header names, rule and path indices and Service names, never annotation
// values.
func (t *translator) translateIngress(ing *networkingv1.Ingress) ([]model.Route, []backendRef, error) {
	requestHeaders, err := headerRules(ing, AnnotationRequestHeaders)
	if err != nil {
		return nil, nil, err
	}
	responseHeaders, err := headerRules(ing, AnnotationResponseHeaders)
	if err != nil {
		return nil, nil, err
	}

	var (
		routes []model.Route
		refs   []backendRef
	)
	for ri, rule := range ing.Spec.Rules {
		// A rule without an http block has nothing to route (the API allows
		// it as a placeholder for future rule kinds).
		if rule.HTTP == nil {
			continue
		}
		host := strings.ToLower(rule.Host)
		for pi, p := range rule.HTTP.Paths {
			ref, err := t.resolveBackend(ing.Namespace, p.Backend)
			if err != nil {
				return nil, nil, fmt.Errorf("rule %d path %d: %w", ri, pi, err)
			}
			routes = append(routes, model.Route{
				Key:             model.RouteKey{Namespace: ing.Namespace, Name: ing.Name, Rule: ri, Path: pi},
				Host:            host,
				Path:            pathOrRoot(p.Path),
				PathType:        pathType(p.PathType),
				Backend:         ref.key,
				RequestHeaders:  requestHeaders,
				ResponseHeaders: responseHeaders,
				Created:         ing.CreationTimestamp.Time,
			})
			refs = append(refs, ref)
		}
	}
	return routes, refs, nil
}

// resolveBackend maps an Ingress backend to a BackendKey and, when the
// Service and its port exist, to the Service port used for endpoint lookup.
//
// A missing Service, or a port that is not on the Service, keeps the route
// with an empty backend so it starts working as soon as the object appears
// (Ingress semantics: 503 until then). A named port cannot be turned into a
// number without the Service, so the key carries port 0 in that case; the
// next rebuild after the Service appears fixes the key.
func (t *translator) resolveBackend(namespace string, b networkingv1.IngressBackend) (backendRef, error) {
	if b.Resource != nil {
		return backendRef{}, errors.New("resource backends are not supported")
	}
	if b.Service == nil {
		return backendRef{}, errors.New("backend has neither a service nor a resource")
	}

	ref := backendRef{key: model.BackendKey{
		Namespace: namespace,
		Service:   b.Service.Name,
		Port:      b.Service.Port.Number,
	}}
	svc, ok := t.services[namespace+"/"+b.Service.Name]
	if !ok {
		return ref, nil
	}
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		// No EndpointSlices exist for an ExternalName Service, and proxying
		// to an arbitrary external name is not something an Ingress does.
		return backendRef{}, fmt.Errorf("service %s is of type ExternalName", objectKey(svc))
	}
	sp := findServicePort(svc, b.Service.Port)
	if sp == nil {
		return ref, nil
	}
	ref.key.Port = sp.Port
	ref.service = svc
	ref.portName = sp.Name
	return ref, nil
}

// findServicePort selects the Service port by name when the backend gives a
// name, otherwise by number. A single unnamed port has the name "".
func findServicePort(svc *corev1.Service, want networkingv1.ServiceBackendPort) *corev1.ServicePort {
	for i := range svc.Spec.Ports {
		sp := &svc.Spec.Ports[i]
		if want.Name != "" {
			if sp.Name == want.Name {
				return sp
			}
			continue
		}
		if sp.Port == want.Number {
			return sp
		}
	}
	return nil
}

// addBackend records the Backend for ref, resolving its endpoints on first
// sight. Later references to the same key reuse the first result: the key
// identifies one Service port, so the endpoints are the same.
func (t *translator) addBackend(ref backendRef) {
	if _, ok := t.backends[ref.key]; ok {
		return
	}
	t.backends[ref.key] = model.Backend{Key: ref.key, Endpoints: t.endpoints(ref)}
}

// endpoints collects the addresses behind ref from every EndpointSlice of its
// Service. Slices with an address type other than IPv4 or IPv6 are ignored
// (FQDN has no defined semantics). Within a slice the port is matched by
// name; the slice port number is the already resolved target port, so
// targetPort is never looked at. Not-ready endpoints are included with
// Ready=false so the router can leave them out of the picker while the
// snapshot still shows them. Addresses seen in several slices are merged, and
// an address is ready if any slice says so.
func (t *translator) endpoints(ref backendRef) []model.Endpoint {
	if ref.service == nil {
		return nil
	}
	ready := make(map[string]bool)
	for _, slice := range t.slices[ref.key.Namespace+"/"+ref.key.Service] {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}
		port, ok := slicePort(slice, ref.portName)
		if !ok {
			continue
		}
		portStr := strconv.Itoa(int(port))
		for _, ep := range slice.Endpoints {
			// The EndpointSlice controller emits exactly one address per
			// endpoint; only addresses[0] is defined to be meaningful.
			if len(ep.Addresses) == 0 {
				continue
			}
			addr := net.JoinHostPort(ep.Addresses[0], portStr)
			// A nil ready condition means true (API reference).
			isReady := ep.Conditions.Ready == nil || *ep.Conditions.Ready
			ready[addr] = ready[addr] || isReady
		}
	}
	if len(ready) == 0 {
		return nil
	}
	eps := make([]model.Endpoint, 0, len(ready))
	for addr, r := range ready {
		eps = append(eps, model.Endpoint{Address: addr, Ready: r})
	}
	// Order is left to Snapshot.Normalize, which Translate always calls.
	return eps
}

// slicePort finds the TCP port named name in slice. A nil name is the empty
// name, which is what a single unnamed Service port maps to. Ports without a
// number or with a protocol other than TCP do not match.
func slicePort(slice *discoveryv1.EndpointSlice, name string) (int32, bool) {
	for _, p := range slice.Ports {
		if p.Port == nil {
			continue
		}
		if p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP {
			continue
		}
		var pn string
		if p.Name != nil {
			pn = *p.Name
		}
		if pn == name {
			return *p.Port, true
		}
	}
	return 0, false
}

// backendList returns the resolved backends as a slice, in map order. The
// caller normalizes the snapshot.
func (t *translator) backendList() []model.Backend {
	if len(t.backends) == 0 {
		return nil
	}
	list := make([]model.Backend, 0, len(t.backends))
	for _, b := range t.backends {
		list = append(list, b)
	}
	return list
}

// headerRules parses one header annotation of ing. An absent annotation is
// no rules. Every header name goes through placeholders.ValidateHeaderName
// and every value through placeholders.Parse, so the controller applies the
// same checks the router will. Errors name the annotation, the line, the
// header name and the underlying error, never the value.
func headerRules(ing *networkingv1.Ingress, annotation string) ([]model.HeaderRule, error) {
	text, ok := ing.Annotations[annotation]
	if !ok {
		return nil, nil
	}
	rules, err := model.ParseHeaderRules(text)
	if err != nil {
		return nil, fmt.Errorf("annotation %s: %w", annotation, err)
	}
	lines := ruleLines(text)
	for i, rule := range rules {
		if err := placeholders.ValidateHeaderName(rule.Name); err != nil {
			return nil, fmt.Errorf("annotation %s: line %d: header %q: %w", annotation, lineOf(lines, i), rule.Name, err)
		}
		if _, err := placeholders.Parse(rule.Value); err != nil {
			return nil, fmt.Errorf("annotation %s: line %d: header %q: %w", annotation, lineOf(lines, i), rule.Name, err)
		}
	}
	return rules, nil
}

// ruleLines returns the 1-based line number of every non-blank line of an
// annotation. model.ParseHeaderRules turns each non-blank line into exactly
// one rule and skips blank ones, so the i-th rule came from lines[i].
func ruleLines(text string) []int {
	var lines []int
	for i, raw := range strings.Split(text, "\n") {
		if strings.TrimSpace(raw) != "" {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// lineOf returns lines[i], or 0 when the index is out of range, which cannot
// happen after a successful ParseHeaderRules but keeps the message harmless.
func lineOf(lines []int, i int) int {
	if i < 0 || i >= len(lines) {
		return 0
	}
	return lines[i]
}

// pathOrRoot maps the empty path (allowed for ImplementationSpecific) to "/".
func pathOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// pathType maps the Ingress path type to the model. Exact stays Exact;
// Prefix and ImplementationSpecific are Prefix, and so is a nil pointer,
// which the API server never produces but a hand-built object might.
func pathType(pt *networkingv1.PathType) model.PathType {
	if pt != nil && *pt == networkingv1.PathTypeExact {
		return model.PathExact
	}
	return model.PathPrefix
}
