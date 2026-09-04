package router

import (
	"sync"
	"testing"
	"time"

	"github.com/RamaHerbin/sluice/internal/model"
)

// snapshotFor returns a valid snapshot routing example.com/ to key, with one
// ready endpoint at addr.
func snapshotFor(key model.BackendKey, addr string) *model.Snapshot {
	return &model.Snapshot{
		Backends: []model.Backend{readyBackend(key, addr)},
		Routes:   []model.Route{newRoute("app", "example.com", "/", model.PathPrefix, key)},
	}
}

// invalidSnapshot returns a snapshot Compile rejects.
func invalidSnapshot() *model.Snapshot {
	return &model.Snapshot{
		Backends: oneBackend(),
		Routes:   []model.Route{newRoute("broken", "example.com", "no-slash", model.PathPrefix, beA)},
	}
}

func TestNewStoreHoldsEmptyTable(t *testing.T) {
	s := NewStore()
	cur := s.Current()
	if cur == nil {
		t.Fatal("Current() is nil")
	}
	if cur.Version != 0 || cur.Hash != "" || cur.Len() != 0 {
		t.Errorf("initial table Version=%d Hash=%q Len=%d, want 0 \"\" 0", cur.Version, cur.Hash, cur.Len())
	}
	if _, ok := cur.Match("example.com", "/"); ok {
		t.Error("initial table matched a request")
	}
}

// Scenario 6: an invalid configuration does not replace the last valid one.
func TestScenario06InvalidConfigKeepsLastValid(t *testing.T) {
	s := NewStore()
	valid, changed, err := s.Apply(snapshotFor(beA, "10.0.0.1:8080"))
	if err != nil || !changed {
		t.Fatalf("Apply(valid) = (%v, %v, %v), want changed with no error", valid, changed, err)
	}
	if valid.Version != 1 || valid.Hash == "" || valid.BuiltAt.IsZero() {
		t.Errorf("published table Version=%d Hash=%q BuiltAt=%v, want 1, non-empty, non-zero", valid.Version, valid.Hash, valid.BuiltAt)
	}
	if s.Current() != valid {
		t.Fatal("Current() is not the table Apply returned")
	}

	tbl, changed, err := s.Apply(invalidSnapshot())
	if err == nil {
		t.Fatal("Apply(invalid) reported no error")
	}
	_ = wantRouteError(t, err, keyOf("broken"))
	if tbl != nil || changed {
		t.Errorf("Apply(invalid) = (%v, %v), want (nil, false)", tbl, changed)
	}
	if s.Current() != valid {
		t.Fatal("Apply(invalid) replaced the current table")
	}
	if s.Current().Version != 1 {
		t.Errorf("Version after rejected Apply = %d, want 1", s.Current().Version)
	}
	r, ok := s.Current().Match("example.com", "/")
	if !ok || r.Service != "svc-a" {
		t.Errorf("last valid table no longer routes example.com/: %v %v", r, ok)
	}

	// The next valid snapshot takes the next version: a rejection consumes
	// no version number.
	next, changed, err := s.Apply(snapshotFor(beB, "10.0.0.2:8080"))
	if err != nil || !changed || next.Version != 2 {
		t.Errorf("Apply(next) = (v%d, %v, %v), want (v2, true, nil)", next.Version, changed, err)
	}
}

func TestStoreApplyIdenticalSnapshotIsNoop(t *testing.T) {
	s := NewStore()
	first, _, err := s.Apply(snapshotFor(beA, "10.0.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}

	again, changed, err := s.Apply(snapshotFor(beA, "10.0.0.1:8080"))
	if err != nil {
		t.Fatalf("Apply(same) error: %v", err)
	}
	if changed {
		t.Error("Apply(same) reported changed=true")
	}
	if again != first || s.Current() != first {
		t.Error("Apply(same) did not return the current table pointer")
	}
	if again.Version != 1 {
		t.Errorf("Version after no-op Apply = %d, want 1", again.Version)
	}

	// The same state in a different order normalizes to the same hash.
	permuted := &model.Snapshot{
		Backends: []model.Backend{readyBackend(beB, "10.0.0.2:8080"), readyBackend(beA, "10.0.0.1:8080")},
		Routes: []model.Route{
			newRoute("b", "b.example.com", "/", model.PathPrefix, beB),
			newRoute("a", "a.example.com", "/", model.PathPrefix, beA),
		},
	}
	sorted := &model.Snapshot{
		Backends: []model.Backend{readyBackend(beA, "10.0.0.1:8080"), readyBackend(beB, "10.0.0.2:8080")},
		Routes: []model.Route{
			newRoute("a", "a.example.com", "/", model.PathPrefix, beA),
			newRoute("b", "b.example.com", "/", model.PathPrefix, beB),
		},
	}
	second, changed, err := s.Apply(permuted)
	if err != nil || !changed || second.Version != 2 {
		t.Fatalf("Apply(permuted) = (v%d, %v, %v), want (v2, true, nil)", second.Version, changed, err)
	}
	third, changed, err := s.Apply(sorted)
	if err != nil || changed || third != second {
		t.Errorf("Apply(sorted twin) = (%p, %v, %v), want (%p, false, nil)", third, changed, err, second)
	}
}

func TestStoreVersionsMonotonic(t *testing.T) {
	s := NewStore()
	snaps := []*model.Snapshot{
		snapshotFor(beA, "10.0.0.1:8080"),
		snapshotFor(beB, "10.0.0.2:8080"),
		snapshotFor(beA, "10.0.0.1:8080"),
		snapshotFor(beB, "10.0.0.2:8080"),
	}
	var want uint64
	for i, snap := range snaps {
		tbl, changed, err := s.Apply(snap)
		if err != nil || !changed {
			t.Fatalf("Apply %d = (%v, %v, %v), want changed", i, tbl, changed, err)
		}
		want++
		if tbl.Version != want {
			t.Errorf("Apply %d Version = %d, want %d", i, tbl.Version, want)
		}
		if s.Current() != tbl {
			t.Errorf("Apply %d: Current() is not the published table", i)
		}
	}
	// A rejection in the middle does not disturb the sequence.
	if _, _, err := s.Apply(invalidSnapshot()); err == nil {
		t.Fatal("Apply(invalid) reported no error")
	}
	tbl, _, err := s.Apply(snapshotFor(beA, "10.0.0.9:8080"))
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Version != want+1 {
		t.Errorf("Version after rejection = %d, want %d", tbl.Version, want+1)
	}
}

func TestStoreApplyNilSnapshot(t *testing.T) {
	s := NewStore()
	tbl, changed, err := s.Apply(nil)
	if err != nil || !changed {
		t.Fatalf("Apply(nil) = (%v, %v, %v), want an empty table at version 1", tbl, changed, err)
	}
	if tbl.Version != 1 || tbl.Len() != 0 || tbl.Hash == "" {
		t.Errorf("Apply(nil) published Version=%d Len=%d Hash=%q", tbl.Version, tbl.Len(), tbl.Hash)
	}
	if again, changed, err := s.Apply(&model.Snapshot{}); err != nil || changed || again != tbl {
		t.Errorf("Apply(empty) after Apply(nil) = (%p, %v, %v), want the same table unchanged", again, changed, err)
	}
}

// TestStoreConcurrentApplyAndMatch is the router-level race hammer: readers
// call Current and Match while a writer alternates Apply between two
// snapshots. The writer sleeps briefly between swaps so the swaps overlap the
// reads for the whole run (architecture.md section 7). Readers check that
// every table they see is coherent and that versions never go backwards.
func TestStoreConcurrentApplyAndMatch(t *testing.T) {
	const (
		swaps   = 1000
		readers = 8
	)
	snapA := snapshotFor(beA, "10.0.0.1:8080")
	snapB := snapshotFor(beB, "10.0.0.2:8080")

	s := NewStore()
	if _, _, err := s.Apply(snapA); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	var reads sync.Map // reader index -> number of reads, for the sanity check
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var last uint64
			var n int
			// The closure reads n when the goroutine exits, not when the
			// defer statement runs.
			defer func() { reads.Store(id, n) }()
			for {
				select {
				case <-done:
					return
				default:
				}
				tbl := s.Current()
				if tbl.Version < last {
					t.Errorf("reader %d saw version %d after %d", id, tbl.Version, last)
					return
				}
				last = tbl.Version
				r, ok := tbl.Match("example.com:8080", "/x")
				if !ok {
					t.Errorf("reader %d: Match found nothing at version %d", id, tbl.Version)
					return
				}
				addr, ok := r.Backend.Pick()
				switch {
				case !ok:
					t.Errorf("reader %d: empty picker at version %d", id, tbl.Version)
					return
				case r.Service == "svc-a" && addr != "10.0.0.1:8080",
					r.Service == "svc-b" && addr != "10.0.0.2:8080",
					r.Service != "svc-a" && r.Service != "svc-b":
					t.Errorf("reader %d: incoherent table at version %d: service %q address %q", id, tbl.Version, r.Service, addr)
					return
				}
				n++
			}
		}(i)
	}

	for i := 0; i < swaps; i++ {
		snap := snapB
		if i%2 == 1 {
			snap = snapA
		}
		if _, changed, err := s.Apply(snap); err != nil || !changed {
			t.Fatalf("swap %d: Apply = (changed=%v, err=%v)", i, changed, err)
		}
		time.Sleep(20 * time.Microsecond)
	}
	close(done)
	wg.Wait()

	if got := s.Current().Version; got != swaps+1 {
		t.Errorf("final Version = %d, want %d", got, swaps+1)
	}
	var total int
	reads.Range(func(_, v any) bool {
		total += v.(int)
		return true
	})
	if total == 0 {
		t.Error("readers performed no reads while the writer was swapping")
	}
}
