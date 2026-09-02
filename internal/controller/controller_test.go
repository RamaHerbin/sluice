package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/router"
)

const waitTimeout = 5 * time.Second

// recordingSink stores every snapshot it receives. It stands in for
// router.Store so these tests do not depend on router.Compile: the returned
// Table only carries Version and Hash. changed mirrors the Store contract:
// false when the hash equals the last accepted one.
type recordingSink struct {
	mu       sync.Mutex
	snaps    []*model.Snapshot
	err      error // when set, Apply fails with it
	version  uint64
	lastHash string
}

func (s *recordingSink) Apply(snap *model.Snapshot) (*router.Table, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, false, s.err
	}
	snap.Normalize()
	s.snaps = append(s.snaps, snap)
	hash := snap.Hash()
	if hash == s.lastHash {
		return &router.Table{Version: s.version, Hash: hash}, false, nil
	}
	s.version++
	s.lastHash = hash
	return &router.Table{Version: s.version, Hash: hash, BuiltAt: time.Now()}, true, nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snaps)
}

// last returns the most recent snapshot, or nil when none was applied.
func (s *recordingSink) last() *model.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snaps) == 0 {
		return nil
	}
	return s.snaps[len(s.snaps)-1]
}

func (s *recordingSink) first() *model.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snaps) == 0 {
		return nil
	}
	return s.snaps[0]
}

// recordingMetrics counts calls.
type recordingMetrics struct {
	applied  atomic.Int64
	rejected atomic.Int64
	skipped  atomic.Int64
	lastVer  atomic.Uint64
}

func (m *recordingMetrics) ConfigApplied(version uint64, _ int) {
	m.applied.Add(1)
	m.lastVer.Store(version)
}
func (m *recordingMetrics) ConfigRejected() { m.rejected.Add(1) }
func (m *recordingMetrics) IngressSkipped() { m.skipped.Add(1) }

// eventually polls cond every 10 ms until it is true or timeout elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runner drives one Controller.Run goroutine in a test.
type runner struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
	timely bool
}

// wait cancels nothing; it waits for Run to return (at most waitTimeout) and
// memoizes the result so the test body and the cleanup can both call it.
func (r *runner) wait() (timely bool, err error) {
	r.once.Do(func() {
		select {
		case r.err = <-r.done:
			r.timely = true
		case <-time.After(waitTimeout):
		}
	})
	return r.timely, r.err
}

// startController builds a controller over a fake clientset seeded with objs,
// runs it in a goroutine and registers a cleanup that cancels the context and
// checks that Run returned nil promptly. It returns the controller, the
// client for later mutations, and the runner.
func startController(t *testing.T, sink Sink, m Metrics, objs ...runtime.Object) (*Controller, kubernetes.Interface, *runner) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	c := New(cs, sink, slog.New(slog.DiscardHandler), m, Options{Debounce: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		timely, err := r.wait()
		if !timely {
			t.Error("Run did not return after cancel")
		} else if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	})
	return c, cs, r
}

func waitSynced(t *testing.T, c *Controller) {
	t.Helper()
	select {
	case <-c.Synced():
	case <-time.After(waitTimeout):
		t.Fatal("Synced not closed in time")
	}
}

func hasRoute(snap *model.Snapshot, key string) bool {
	if snap == nil {
		return false
	}
	for _, r := range snap.Routes {
		if r.Key.String() == key {
			return true
		}
	}
	return false
}

func seed() []runtime.Object {
	return []runtime.Object{
		newIngressClass(DefaultIngressClass, DefaultControllerName),
		echoService(),
		echoSlice(),
	}
}

func webIngress() *networkingv1.Ingress {
	return newIngress(ns, "web", httpRule("echo.example.com", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
}

func TestNewDoesNotStart(t *testing.T) {
	c := New(fake.NewClientset(), &recordingSink{}, nil, nil, Options{})
	if c == nil {
		t.Fatal("New returned nil")
	}
	select {
	case <-c.Synced():
		t.Fatal("Synced closed before Run")
	default:
	}
	if c.opts.IngressClass != DefaultIngressClass || c.opts.ControllerName != DefaultControllerName || c.opts.Debounce != defaultDebounce {
		t.Fatalf("defaults not applied: %+v", c.opts)
	}
	if c.log == nil || c.metrics == nil {
		t.Fatal("nil logger or metrics not defaulted")
	}
}

func TestMarkDirtyNeverBlocks(t *testing.T) {
	c := New(fake.NewClientset(), &recordingSink{}, nil, nil, Options{})
	for i := 0; i < 5; i++ {
		c.markDirty()
	}
	if len(c.kick) != 1 {
		t.Fatalf("kick channel holds %d items, want 1", len(c.kick))
	}
}

func TestEndpointSliceServiceKey(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{}
	slice.Namespace = "demo"
	slice.Name = "echo-abc"
	slice.Labels = map[string]string{discoveryv1.LabelServiceName: "echo"}
	keys, err := endpointSliceServiceKey(slice)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "demo/echo" {
		t.Fatalf("keys = %v, want [demo/echo]", keys)
	}
	if got := objectKey(slice); got != "demo/echo-abc" {
		t.Fatalf("objectKey = %q", got)
	}
	if _, err := endpointSliceServiceKey("nope"); err == nil {
		t.Fatal("expected error for wrong type")
	}
	slice.Labels = nil
	if keys, err := endpointSliceServiceKey(slice); err != nil || len(keys) != 0 {
		t.Fatalf("unlabelled slice: keys %v err %v, want none", keys, err)
	}
}

// TestScenario14SyncedAfterFirstApply: Synced is closed only after the
// informers have synced and the first Apply happened, and that first
// snapshot already holds the seeded route.
func TestScenario14SyncedAfterFirstApply(t *testing.T) {
	sink := &recordingSink{}
	metrics := &recordingMetrics{}
	c, _, _ := startController(t, sink, metrics, append(seed(), webIngress())...)

	waitSynced(t, c)

	// Observed at the instant Synced fired: the first Apply is already
	// recorded, and it was built from the fully synced cache.
	if sink.count() < 1 {
		t.Fatal("Synced closed before the first Apply")
	}
	if !hasRoute(sink.first(), "demo/web#r0p0") {
		t.Fatalf("first snapshot lacks the seeded route: %+v", sink.first())
	}
	if len(sink.first().Backends) != 1 || len(sink.first().Backends[0].Endpoints) != 2 {
		t.Fatalf("first snapshot backends = %+v, want echo with two endpoints", sink.first().Backends)
	}
	if metrics.applied.Load() != 1 || metrics.lastVer.Load() != 1 {
		t.Fatalf("ConfigApplied calls = %d (version %d), want 1 (version 1)", metrics.applied.Load(), metrics.lastVer.Load())
	}
}

// TestSyncedEvenWhenFirstApplyIsRejected: readiness is "synced and first
// apply attempted", so a rejected first snapshot still closes Synced and is
// counted as rejected.
func TestSyncedEvenWhenFirstApplyIsRejected(t *testing.T) {
	sink := &recordingSink{err: errors.New("compile failed")}
	metrics := &recordingMetrics{}
	c, _, _ := startController(t, sink, metrics, append(seed(), webIngress())...)

	waitSynced(t, c)
	if metrics.rejected.Load() < 1 {
		t.Fatal("ConfigRejected not called")
	}
	if metrics.applied.Load() != 0 {
		t.Fatal("ConfigApplied called although Apply failed")
	}
}

// TestScenario05NewIngressAppliesRoute: an Ingress created after the initial
// sync leads to a new Apply that contains its route, with no restart.
func TestScenario05NewIngressAppliesRoute(t *testing.T) {
	sink := &recordingSink{}
	c, cs, _ := startController(t, sink, &recordingMetrics{}, seed()...)
	waitSynced(t, c)

	if hasRoute(sink.last(), "demo/web#r0p0") {
		t.Fatal("route present before the Ingress was created")
	}
	if _, err := cs.NetworkingV1().Ingresses(ns).Create(context.Background(), webIngress(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, waitTimeout, func() bool { return hasRoute(sink.last(), "demo/web#r0p0") })

	last := sink.last()
	if len(last.Backends) != 1 || len(last.Backends[0].Endpoints) != 2 {
		t.Fatalf("backends = %+v, want echo with two endpoints", last.Backends)
	}
}

// TestScenario13DeleteIngressRemovesRoute: deleting the Ingress leads to an
// Apply without its route.
func TestScenario13DeleteIngressRemovesRoute(t *testing.T) {
	sink := &recordingSink{}
	c, cs, _ := startController(t, sink, &recordingMetrics{}, append(seed(), webIngress())...)
	waitSynced(t, c)
	if !hasRoute(sink.last(), "demo/web#r0p0") {
		t.Fatal("seeded route missing after sync")
	}

	if err := cs.NetworkingV1().Ingresses(ns).Delete(context.Background(), "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, waitTimeout, func() bool {
		last := sink.last()
		return last != nil && len(last.Routes) == 0
	})
	// The Service is still there but nothing references it any more.
	if last := sink.last(); len(last.Backends) != 0 {
		t.Fatalf("backends = %+v, want none", last.Backends)
	}
}

// TestEndpointSliceUpdateRebuilds is the informer half of scenario 4: when a
// slice loses an endpoint, the next snapshot no longer lists it.
func TestEndpointSliceUpdateRebuilds(t *testing.T) {
	sink := &recordingSink{}
	c, cs, _ := startController(t, sink, &recordingMetrics{}, append(seed(), webIngress())...)
	waitSynced(t, c)

	slice := echoSlice()
	slice.Endpoints = slice.Endpoints[:1]
	if _, err := cs.DiscoveryV1().EndpointSlices(ns).Update(context.Background(), slice, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, waitTimeout, func() bool {
		last := sink.last()
		return last != nil && len(last.Backends) == 1 && len(last.Backends[0].Endpoints) == 1
	})
	if got := sink.last().Backends[0].Endpoints[0].Address; got != "10.0.0.1:8080" {
		t.Fatalf("remaining endpoint = %s, want 10.0.0.1:8080", got)
	}
}

// TestSkippedIngressIsCountedAndOthersApplied: a bad Ingress is logged and
// counted while the good one is served.
func TestSkippedIngressIsCountedAndOthersApplied(t *testing.T) {
	bad := withAnnotations(
		newIngress(ns, "bad", httpRule("bad.example.com", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80)))),
		AnnotationRequestHeaders, "X-Route: {nope}",
	)
	sink := &recordingSink{}
	metrics := &recordingMetrics{}
	c, _, _ := startController(t, sink, metrics, append(seed(), webIngress(), bad)...)
	waitSynced(t, c)

	first := sink.first()
	if !hasRoute(first, "demo/web#r0p0") || hasRoute(first, "demo/bad#r0p0") {
		t.Fatalf("routes = %v, want only demo/web#r0p0", routeKeys(first))
	}
	if metrics.skipped.Load() != 1 {
		t.Fatalf("IngressSkipped calls = %d, want 1", metrics.skipped.Load())
	}
}

// TestNamespaceOptionRestrictsWatch: with Options.Namespace set, an Ingress
// in another namespace is never seen.
func TestNamespaceOptionRestrictsWatch(t *testing.T) {
	elsewhere := newIngress("elsewhere", "web", httpRule("", servicePath("/", networkingv1.PathTypePrefix, svc, byNumber(80))))
	cs := fake.NewClientset(append(seed(), webIngress(), elsewhere)...)
	sink := &recordingSink{}
	c := New(cs, sink, slog.New(slog.DiscardHandler), NopMetrics{}, Options{Namespace: ns, Debounce: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})
	waitSynced(t, c)
	if keys := routeKeys(sink.first()); len(keys) != 1 || keys[0] != "demo/web#r0p0" {
		t.Fatalf("routes = %v, want only demo/web#r0p0", keys)
	}
}

// TestRunReturnsNilOnCancel: cancelling the context makes Run return nil
// promptly, whether it is idle or in the middle of a debounce.
func TestRunReturnsNilOnCancel(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		c, _, r := startController(t, &recordingSink{}, &recordingMetrics{}, seed()...)
		waitSynced(t, c)
		r.cancel()
		timely, err := r.wait()
		if !timely {
			t.Fatal("Run did not return after cancel")
		}
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	})

	t.Run("during debounce", func(t *testing.T) {
		cs := fake.NewClientset(seed()...)
		c := New(cs, &recordingSink{}, slog.New(slog.DiscardHandler), NopMetrics{}, Options{Debounce: time.Hour})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- c.Run(ctx) }()
		waitSynced(t, c)
		// Kick so the loop enters its hour-long debounce, then cancel.
		c.markDirty()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v, want nil", err)
			}
		case <-time.After(waitTimeout):
			t.Fatal("Run stuck in debounce after cancel")
		}
	})

	t.Run("before sync completes", func(t *testing.T) {
		c := New(fake.NewClientset(seed()...), &recordingSink{}, slog.New(slog.DiscardHandler), NopMetrics{}, Options{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Run(ctx); err != nil {
			t.Fatalf("Run with a cancelled context returned %v, want nil", err)
		}
	})
}

// TestRebuildCoalescesBurst: many kicks in one debounce window produce one
// rebuild, and an unchanged cache does not bump the version.
func TestRebuildCoalescesBurst(t *testing.T) {
	sink := &recordingSink{}
	metrics := &recordingMetrics{}
	c, _, _ := startController(t, sink, metrics, append(seed(), webIngress())...)
	waitSynced(t, c)
	before := sink.count()

	for i := 0; i < 10; i++ {
		c.markDirty()
	}
	eventually(t, waitTimeout, func() bool { return sink.count() > before })
	// Give a second window a chance to fire spuriously.
	time.Sleep(50 * time.Millisecond)
	if got := sink.count(); got != before+1 {
		t.Fatalf("Apply calls after burst = %d, want %d", got, before+1)
	}
	if metrics.applied.Load() != 1 {
		t.Fatalf("ConfigApplied calls = %d, want 1 (unchanged snapshot must not count)", metrics.applied.Load())
	}
}
