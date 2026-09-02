package upstream

import (
	"math"
	"reflect"
	"sync"
	"testing"
)

// picks calls Pick n times and returns the addresses in order, failing the
// test if any call reports ok=false.
func picks(t *testing.T, p Picker, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		addr, ok := p.Pick()
		if !ok {
			t.Fatalf("Pick %d reported ok=false on a non-empty picker", i)
		}
		out = append(out, addr)
	}
	return out
}

// TestScenario03RoundRobinTwoAddresses covers scenario 3 at the picker level:
// two endpoints receive requests strictly alternating, starting at index 0.
func TestScenario03RoundRobinTwoAddresses(t *testing.T) {
	p := NewRoundRobin([]string{"a", "b"})
	if p.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", p.Len())
	}
	got := picks(t, p, 6)
	want := []string{"a", "b", "a", "b", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sequence %q, want %q", got, want)
	}
}

func TestRoundRobinThreeAddresses(t *testing.T) {
	p := NewRoundRobin([]string{"a", "b", "c"})
	if p.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", p.Len())
	}
	got := picks(t, p, 7)
	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sequence %q, want %q", got, want)
	}
}

func TestRoundRobinSingleAddress(t *testing.T) {
	p := NewRoundRobin([]string{"only"})
	for i, addr := range picks(t, p, 3) {
		if addr != "only" {
			t.Errorf("Pick %d = %q, want %q", i, addr, "only")
		}
	}
}

func TestRoundRobinEmpty(t *testing.T) {
	for _, addrs := range [][]string{nil, {}} {
		p := NewRoundRobin(addrs)
		if p.Len() != 0 {
			t.Errorf("Len() = %d for %#v, want 0", p.Len(), addrs)
		}
		for i := 0; i < 3; i++ {
			addr, ok := p.Pick()
			if ok || addr != "" {
				t.Errorf("Pick on empty picker returned (%q, %v), want (\"\", false)", addr, ok)
			}
		}
	}
}

func TestRoundRobinCopiesInput(t *testing.T) {
	in := []string{"a", "b"}
	p := NewRoundRobin(in)
	in[0] = "mutated"
	in = append(in, "c")
	if len(in) != 3 || p.Len() != 2 {
		t.Fatalf("Len() = %d after appending to the input (now %d long), want 2", p.Len(), len(in))
	}
	got := picks(t, p, 4)
	want := []string{"a", "b", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sequence %q after mutating the input, want %q", got, want)
	}
}

func TestRoundRobinCounterWrap(t *testing.T) {
	// dataplane.md section 7: around the uint32 wrap the index sequence is
	// MaxUint32-1, MaxUint32, 0, 1. Two addresses keep alternating; three
	// addresses repeat one address once per wrap. Both are harmless.
	two := NewRoundRobin([]string{"a", "b"}).(*roundRobin)
	two.next.Store(math.MaxUint32 - 1)
	if got, want := picks(t, two, 4), []string{"a", "b", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("two addresses around the wrap: %q, want %q", got, want)
	}
	three := NewRoundRobin([]string{"a", "b", "c"}).(*roundRobin)
	three.next.Store(math.MaxUint32 - 1)
	if got, want := picks(t, three, 4), []string{"c", "a", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("three addresses around the wrap: %q, want %q", got, want)
	}
}

// TestRoundRobinConcurrent runs many goroutines against one picker under the
// race detector. The atomic counter hands every call a distinct sequence
// number, so the addresses are spread exactly: each one receives either
// floor(total/len) or ceil(total/len) picks. The tolerance is therefore one.
func TestRoundRobinConcurrent(t *testing.T) {
	const (
		goroutines = 32
		perG       = 1000
	)
	addrs := []string{"a", "b", "c"}
	p := NewRoundRobin(addrs)

	counts := make([]map[string]int, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		counts[g] = make(map[string]int, len(addrs))
		wg.Add(1)
		go func(mine map[string]int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				addr, ok := p.Pick()
				if !ok {
					mine["!ok"]++
					continue
				}
				mine[addr]++
			}
		}(counts[g])
	}
	wg.Wait()

	total := make(map[string]int, len(addrs))
	for _, m := range counts {
		for k, v := range m {
			total[k] += v
		}
	}
	if total["!ok"] != 0 {
		t.Fatalf("%d picks reported ok=false", total["!ok"])
	}
	sum := 0
	for _, a := range addrs {
		sum += total[a]
	}
	if sum != goroutines*perG {
		t.Fatalf("counted %d picks, want %d", sum, goroutines*perG)
	}
	share := goroutines * perG / len(addrs)
	for _, a := range addrs {
		if d := total[a] - share; d < -1 || d > 1 {
			t.Errorf("address %q picked %d times, want %d within 1", a, total[a], share)
		}
	}
}
