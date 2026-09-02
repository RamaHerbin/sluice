// Package upstream selects which endpoint of a backend receives a request.
//
// Pickers are created at configuration compile time and live inside the
// immutable routing table. Their only mutable state is a counter, so they are
// safe for concurrent use without locks.
package upstream

// Picker chooses an upstream address for one request.
type Picker interface {
	// Pick returns the next address. ok is false when the picker is empty.
	Pick() (addr string, ok bool)
	// Len returns the number of addresses the picker rotates over.
	Len() int
}

// NewRoundRobin returns a Picker rotating over addrs in order, starting at the
// first one. addrs is copied, so the caller may reuse its slice. An empty
// addrs yields a Picker whose Pick always reports ok=false.
func NewRoundRobin(addrs []string) Picker {
	// TODO(wave-2, proxy+upstream agent): implement with atomic.Uint32.
	return roundRobin{}
}

type roundRobin struct{}

func (roundRobin) Pick() (string, bool) { return "", false }
func (roundRobin) Len() int             { return 0 }
