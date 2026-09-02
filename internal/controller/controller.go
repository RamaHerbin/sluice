// Package controller is the control plane: it watches Kubernetes objects with
// informers, translates them into a model.Snapshot and publishes it.
//
// This is the only package allowed to import k8s.io/*. Translate is a pure
// function over plain slices of typed objects so it is testable without any
// client; the informer plumbing around it is deliberately thin.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/router"
)

const (
	// DefaultControllerName is the value of IngressClass.spec.controller that
	// sluice claims.
	DefaultControllerName = "sluice.dev/ingress-controller"
	// DefaultIngressClass is the IngressClass name used when none is given.
	DefaultIngressClass = "sluice"
	// AnnotationRequestHeaders holds header rules for the upstream request,
	// one "Name: value" per line. Values may contain placeholders.
	AnnotationRequestHeaders = "sluice.dev/request-headers"
	// AnnotationResponseHeaders holds header rules for the client response.
	AnnotationResponseHeaders = "sluice.dev/response-headers"
	// LegacyClassAnnotation is the deprecated way to select an ingress class.
	LegacyClassAnnotation = "kubernetes.io/ingress.class"
	// serviceIndex is the EndpointSlice indexer name: "namespace/serviceName".
	serviceIndex = "service"
)

// Input is everything Translate reads: plain slices of typed objects, so
// tests need no client and no informer.
type Input struct {
	// IngressClass is the class name sluice claims (see Options).
	IngressClass string
	// ControllerName is the IngressClass.spec.controller value sluice claims.
	ControllerName string

	IngressClasses []*networkingv1.IngressClass
	Ingresses      []*networkingv1.Ingress
	Services       []*corev1.Service
	Slices         []*discoveryv1.EndpointSlice
}

// Warning describes an Ingress or rule that was skipped during translation.
type Warning struct {
	Object string // "namespace/name"
	Reason string
}

// Translate converts the claimed Ingresses into a snapshot. It never fails:
// an Ingress that cannot be translated is skipped and reported as a Warning,
// so one bad object never blocks updates for the rest of the cluster. Header
// annotations are parsed with model.ParseHeaderRules and their values with
// placeholders.Parse; a resource backend or an ExternalName Service also
// skips the Ingress. A missing Service or zero ready endpoints keeps the
// route with an empty backend, which the proxy answers with 503.
func Translate(in Input) (*model.Snapshot, []Warning) {
	// TODO(wave-2, controller agent): implement.
	return &model.Snapshot{}, nil
}

// Sink receives snapshots. *router.Store satisfies it.
type Sink interface {
	Apply(*model.Snapshot) (*router.Table, bool, error)
}

// Metrics is the consumer-side metrics surface the controller needs.
type Metrics interface {
	ConfigApplied(version uint64, routes int)
	ConfigRejected()
	IngressSkipped()
}

// NopMetrics discards every observation.
type NopMetrics struct{}

func (NopMetrics) ConfigApplied(uint64, int) {}
func (NopMetrics) ConfigRejected()           {}
func (NopMetrics) IngressSkipped()           {}

// Options configures a Controller.
type Options struct {
	IngressClass   string        // default DefaultIngressClass
	ControllerName string        // default DefaultControllerName
	Namespace      string        // "" watches all namespaces
	Debounce       time.Duration // default 100ms
}

// Controller runs the informers and the rebuild loop. Create it with New.
type Controller struct {
	client  kubernetes.Interface
	sink    Sink
	log     *slog.Logger
	metrics Metrics
	opts    Options
	synced  chan struct{}
	factory informers.SharedInformerFactory
	// TODO(wave-2, controller agent): listers, kick channel.
}

// New builds a Controller. Nothing starts until Run is called.
func New(cs kubernetes.Interface, sink Sink, log *slog.Logger, m Metrics, o Options) *Controller {
	// TODO(wave-2, controller agent): apply defaults, build informers.
	return &Controller{client: cs, sink: sink, log: log, metrics: m, opts: o, synced: make(chan struct{})}
}

// Run starts the informers, waits for the initial cache sync, performs the
// first build, closes Synced and then rebuilds on every change until ctx is
// cancelled. It returns nil on cancellation and an error when the initial
// sync fails.
func (c *Controller) Run(ctx context.Context) error {
	// TODO(wave-2, controller agent): implement.
	return errors.New("controller: not implemented")
}

// Synced is closed once the first snapshot has been built from a fully
// synced cache. Readiness must wait for it.
func (c *Controller) Synced() <-chan struct{} { return c.synced }

// NewClientset builds a clientset from the kubeconfig at path, or from the
// in-cluster service account when path is empty.
func NewClientset(kubeconfig string) (kubernetes.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes clientset: %w", err)
	}
	return cs, nil
}

// objectKey renders "namespace/name" for warnings and logs.
func objectKey(obj metav1.Object) string {
	return obj.GetNamespace() + "/" + obj.GetName()
}

// endpointSliceServiceKey is the cache.IndexFunc for serviceIndex. It indexes
// an EndpointSlice under "namespace/serviceName" using the
// kubernetes.io/service-name label, so all slices of a Service are found with
// one ByIndex call.
func endpointSliceServiceKey(obj any) ([]string, error) {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil, fmt.Errorf("expected *discoveryv1.EndpointSlice, got %T", obj)
	}
	svc := slice.Labels[discoveryv1.LabelServiceName]
	if svc == "" {
		return nil, nil
	}
	return []string{slice.Namespace + "/" + svc}, nil
}

// Ensure the index function has the type client-go expects.
var _ cache.IndexFunc = endpointSliceServiceKey
