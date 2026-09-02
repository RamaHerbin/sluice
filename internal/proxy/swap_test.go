package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/router"
)

// Scenario 7 and the atomic publication proof (docs/design/architecture.md
// section 3): 16 goroutines send 500 requests each through one Handler while
// the Store keeps alternating between a table pointing at backend "A" and one
// pointing at "B" for the whole duration of the traffic. Every response must
// be a 200 whose body is exactly "A" or "B": a request never observes two
// tables, and no swap is lost or torn. Run with -race.
func TestScenario07SwapUnderLoad(t *testing.T) {
	a, hitsA := textBackend(t, "A")
	b, hitsB := textBackend(t, "B")
	snapA := snapshot([]model.Backend{backend(beA, a)}, route("app", "example.com", "/", beA))
	snapB := snapshot([]model.Backend{backend(beB, b)}, route("app", "example.com", "/", beB))
	store := newStore(t, snapA)
	h := newHandler(t, store, Options{})

	const workers, perWorker = 16, 500
	var countA, countB, bad atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				rec := serve(h, "http://example.com/", nil)
				switch body := rec.Body.String(); {
				case rec.Code != http.StatusOK:
					bad.Add(1)
					t.Errorf("status %d body %q", rec.Code, body)
				case body == "A":
					countA.Add(1)
				case body == "B":
					countB.Add(1)
				default:
					bad.Add(1)
					t.Errorf("body %q is neither A nor B", body)
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// The swapper runs on the test goroutine until the workers finish. The
	// short sleep keeps this from being a CPU spin while still producing
	// thousands of swaps over the traffic; 1000 back-to-back stores would
	// finish before the first request (docs/design/dataplane.md section 8).
	snaps := [2]*model.Snapshot{snapB, snapA}
	swaps := 0
swapping:
	for i := 0; ; i++ {
		select {
		case <-done:
			break swapping
		default:
		}
		if _, changed, err := store.Apply(snaps[i%2]); err != nil {
			t.Fatalf("Apply: %v", err)
		} else if changed {
			swaps++
		}
		time.Sleep(100 * time.Microsecond)
	}

	total := countA.Load() + countB.Load()
	t.Logf("responses A=%d B=%d bad=%d swaps=%d", countA.Load(), countB.Load(), bad.Load(), swaps)
	if bad.Load() != 0 || total != workers*perWorker {
		t.Errorf("A+B = %d with %d bad responses, want %d and 0", total, bad.Load(), workers*perWorker)
	}
	if countA.Load() == 0 || countB.Load() == 0 {
		t.Error("one backend never answered: the swaps did not overlap the traffic")
	}
	if swaps < 50 {
		t.Errorf("only %d swaps happened during the traffic, want at least 50", swaps)
	}
	if hitsA.Load() != countA.Load() || hitsB.Load() != countB.Load() {
		t.Errorf("backend hits A=%d B=%d differ from client counts A=%d B=%d",
			hitsA.Load(), hitsB.Load(), countA.Load(), countB.Load())
	}
}

// A request already inside a slow backend keeps that backend even when the
// table is swapped to a fast one while it waits; the next request gets the
// new backend.
func TestSwapMidFlightKeepsOldBackend(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		signal(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		respond(w, "slow")
	})
	fast, _ := textBackend(t, "fast")
	store := newStore(t, snapshot([]model.Backend{backend(beA, slow)}, route("app", "example.com", "/", beA)))
	h := newHandler(t, store, Options{})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- serve(h, "http://example.com/", nil) }()
	wait(t, entered, "slow backend to receive the request")

	tbl := mustApply(t, store, snapshot([]model.Backend{backend(beB, fast)}, route("app", "example.com", "/", beB)))
	if store.Current() != tbl {
		t.Fatal("the swap was not published")
	}
	close(release)

	rec := wait(t, first, "in-flight response")
	if rec.Code != http.StatusOK || rec.Body.String() != "slow" {
		t.Errorf("in-flight response: status %d body %q, want 200 slow", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "http://example.com/", nil); rec.Code != http.StatusOK || rec.Body.String() != "fast" {
		t.Errorf("next response: status %d body %q, want 200 fast", rec.Code, rec.Body.String())
	}
}

// A Provider whose table is nil (router.Store never does this) gets a 404
// rather than a panic.
func TestNilTableIs404(t *testing.T) {
	h := newHandler(t, nilProvider{}, Options{})
	if rec := serve(h, "http://example.com/", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

type nilProvider struct{}

func (nilProvider) Current() *router.Table { return nil }
