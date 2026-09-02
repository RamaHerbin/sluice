// Package controller is the control plane: it watches Kubernetes objects with
// informers, translates them into a model.Snapshot and publishes it.
//
// This is the only package allowed to import k8s.io/*. Translate is a pure
// function over plain slices of typed objects so it is testable without any
// client; the informer plumbing around it is deliberately thin.
package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
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
	// defaultDebounce is the delay between the first change notification and
	// the rebuild, so a burst of events costs one rebuild.
	defaultDebounce = 100 * time.Millisecond
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
//
// An empty IngressClass or ControllerName falls back to the defaults. The
// input slices are not modified. Ingresses are processed in namespace/name
// order and the snapshot is normalized, so the output does not depend on the
// order of the inputs. Warnings never contain annotation values.
func Translate(in Input) (*model.Snapshot, []Warning) {
	t := newTranslator(in)

	// Sort a copy: lister output comes in map order, and warnings should be
	// as deterministic as the snapshot.
	ingresses := slices.Clone(in.Ingresses)
	slices.SortFunc(ingresses, func(a, b *networkingv1.Ingress) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})

	snap := &model.Snapshot{}
	var warnings []Warning
	for _, ing := range ingresses {
		if ing == nil || !t.claims(ing) {
			continue
		}
		routes, refs, err := t.translateIngress(ing)
		if err != nil {
			warnings = append(warnings, Warning{Object: objectKey(ing), Reason: err.Error()})
			continue
		}
		snap.Routes = append(snap.Routes, routes...)
		for _, ref := range refs {
			t.addBackend(ref)
		}
	}
	snap.Backends = t.backendList()
	snap.Normalize()
	return snap, warnings
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

	ingresses networkinglisters.IngressLister
	classes   networkinglisters.IngressClassLister
	services  corelisters.ServiceLister
	slices    discoverylisters.EndpointSliceLister

	// kick has a capacity of one. Informer handlers do a non-blocking send on
	// it and nothing else; the rebuild loop drains it. Dropping a send when
	// the channel is already full loses nothing because every rebuild reads
	// the whole cache.
	kick chan struct{}
	// syncOnce guards the close of synced so a second Run cannot panic.
	syncOnce sync.Once
	// setupErr collects failures from New that can only be reported by Run,
	// which has an error return. It is nil in practice: client-go rejects an
	// indexer or a handler only on an informer that already started.
	setupErr error
}

// New builds a Controller. Nothing starts until Run is called.
func New(cs kubernetes.Interface, sink Sink, log *slog.Logger, m Metrics, o Options) *Controller {
	if o.IngressClass == "" {
		o.IngressClass = DefaultIngressClass
	}
	if o.ControllerName == "" {
		o.ControllerName = DefaultControllerName
	}
	if o.Debounce <= 0 {
		o.Debounce = defaultDebounce
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if m == nil {
		m = NopMetrics{}
	}

	// Resync 0: a resync replays the local cache without contacting the API
	// server, and a full-rebuild design gains nothing from it.
	var fopts []informers.SharedInformerOption
	if o.Namespace != "" {
		fopts = append(fopts, informers.WithNamespace(o.Namespace))
	}
	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0, fopts...)

	ingresses := factory.Networking().V1().Ingresses()
	classes := factory.Networking().V1().IngressClasses()
	services := factory.Core().V1().Services()
	endpointSlices := factory.Discovery().V1().EndpointSlices()

	c := &Controller{
		client:    cs,
		sink:      sink,
		log:       log,
		metrics:   m,
		opts:      o,
		synced:    make(chan struct{}),
		factory:   factory,
		ingresses: ingresses.Lister(),
		classes:   classes.Lister(),
		services:  services.Lister(),
		slices:    endpointSlices.Lister(),
		kick:      make(chan struct{}, 1),
	}

	if err := endpointSlices.Informer().AddIndexers(cache.Indexers{serviceIndex: endpointSliceServiceKey}); err != nil {
		c.setupErr = errors.Join(c.setupErr, fmt.Errorf("add EndpointSlice service indexer: %w", err))
	}

	// Handlers run on informer goroutines. They must never touch the store
	// or build anything: they only mark the cache dirty.
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.markDirty() },
		UpdateFunc: func(any, any) { c.markDirty() },
		// DeleteFunc may receive a cache.DeletedFinalStateUnknown tombstone
		// instead of the object when a delete was missed during a watch
		// disconnect and only noticed at the next relist. The payload is
		// ignored either way because a rebuild reads the whole cache, so no
		// type assertion is needed here.
		DeleteFunc: func(any) { c.markDirty() },
	}
	for name, inf := range map[string]cache.SharedIndexInformer{
		"ingresses":      ingresses.Informer(),
		"ingressclasses": classes.Informer(),
		"services":       services.Informer(),
		"endpointslices": endpointSlices.Informer(),
	} {
		if _, err := inf.AddEventHandler(handler); err != nil {
			c.setupErr = errors.Join(c.setupErr, fmt.Errorf("add %s event handler: %w", name, err))
		}
	}
	return c
}

// markDirty is the only thing an informer handler does: a non-blocking send
// on the size-1 kick channel.
func (c *Controller) markDirty() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run starts the informers, waits for the initial cache sync, performs the
// first build, closes Synced and then rebuilds on every change until ctx is
// cancelled. It returns nil on cancellation and an error when the initial
// sync fails.
func (c *Controller) Run(ctx context.Context) error {
	if c.setupErr != nil {
		return fmt.Errorf("controller: set up informers: %w", c.setupErr)
	}

	c.factory.Start(ctx.Done())

	// Shutdown waits for the informer goroutines, and they only exit once
	// ctx is done. It is therefore called on the cancellation paths only;
	// on any other early return the informers keep running until the caller
	// cancels ctx, which is the lifecycle they were started with.
	stop := func() error {
		c.factory.Shutdown()
		return nil
	}

	if err := c.waitForCacheSync(ctx); err != nil {
		if ctx.Err() != nil {
			// The sync did not complete because we were asked to stop.
			return stop()
		}
		return err
	}

	c.rebuild()
	c.syncOnce.Do(func() { close(c.synced) })

	for {
		select {
		case <-ctx.Done():
			return stop()
		case <-c.kick:
		}

		// Debounce: sleep, then drain whatever arrived meanwhile, then one
		// rebuild. Sleep-then-drain avoids the timer reset races of a
		// coalescing timer and keeps the loop trivially correct.
		timer := time.NewTimer(c.opts.Debounce)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stop()
		case <-timer.C:
		}
		select {
		case <-c.kick:
		default:
		}
		c.rebuild()
	}
}

// waitForCacheSync blocks until every started informer has synced or ctx is
// done. It returns an error naming the informers that did not sync.
func (c *Controller) waitForCacheSync(ctx context.Context) error {
	var failed []string
	for typ, ok := range c.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			failed = append(failed, typ.String())
		}
	}
	if len(failed) == 0 {
		return nil
	}
	sort.Strings(failed)
	return fmt.Errorf("controller: initial cache sync failed for %s", strings.Join(failed, ", "))
}

// rebuild reads the listers, translates everything and hands the snapshot to
// the sink. It runs on the Run goroutine only. Errors are logged, never
// returned: the previous table keeps serving and the next change retries.
func (c *Controller) rebuild() {
	in, err := c.listInput()
	if err != nil {
		c.log.Error("list objects from cache", "err", err)
		return
	}

	snap, warnings := Translate(in)
	for _, w := range warnings {
		c.log.Warn("ingress skipped", "object", w.Object, "reason", w.Reason)
		c.metrics.IngressSkipped()
	}

	table, changed, err := c.sink.Apply(snap)
	if err != nil {
		c.log.Error("config rejected", "err", err)
		c.metrics.ConfigRejected()
		return
	}

	// The sink contract gives a non-nil table on success; the guards below
	// only keep a lenient test sink from crashing the loop.
	var (
		version uint64
		hash    string
	)
	routes := len(snap.Routes)
	if table != nil {
		version, hash, routes = table.Version, table.Hash, table.Len()
	}
	if !changed {
		c.log.Debug("config unchanged", "version", version, "hash", hash, "routes", routes)
		return
	}
	c.log.Info("config applied", "version", version, "hash", hash, "routes", routes)
	c.metrics.ConfigApplied(version, routes)
}

// listInput reads every object kind from the informer caches. Listers over an
// indexer do not fail in practice, but the signatures carry an error, so it
// is propagated rather than ignored.
func (c *Controller) listInput() (Input, error) {
	in := Input{IngressClass: c.opts.IngressClass, ControllerName: c.opts.ControllerName}
	var err error
	if in.IngressClasses, err = c.classes.List(labels.Everything()); err != nil {
		return Input{}, fmt.Errorf("list ingressclasses: %w", err)
	}
	if in.Ingresses, err = c.ingresses.List(labels.Everything()); err != nil {
		return Input{}, fmt.Errorf("list ingresses: %w", err)
	}
	if in.Services, err = c.services.List(labels.Everything()); err != nil {
		return Input{}, fmt.Errorf("list services: %w", err)
	}
	if in.Slices, err = c.slices.List(labels.Everything()); err != nil {
		return Input{}, fmt.Errorf("list endpointslices: %w", err)
	}
	return in, nil
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
