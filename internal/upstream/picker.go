// Package upstream selects which endpoint of a backend receives a request.
//
// Pickers are created at configuration compile time and live inside the
// immutable routing table. Their only mutable state is a counter, so they are
// safe for concurrent use without locks.
package upstream

import (
	"slices"
	"sync/atomic"
)

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
	return &roundRobin{addrs: slices.Clone(addrs)}
}

// roundRobin hands out addresses in order. The counter is the only mutable
// cell inside a compiled routing table; it is never reset, and the modulo
// makes the uint32 wrap-around harmless (see docs/design/dataplane.md
// section 7 for the exact sequence around the wrap).
type roundRobin struct {
	addrs []string
	next  atomic.Uint32
}

// Pick returns addrs[i % len] where i is the number of earlier calls, so a
// fresh picker starts at index 0.
func (r *roundRobin) Pick() (string, bool) {
	n := len(r.addrs)
	if n == 0 {
		return "", false
	}
	i := r.next.Add(1) - 1
	return r.addrs[i%uint32(n)], true
}

// Len returns the number of addresses the picker rotates over.
func (r *roundRobin) Len() int { return len(r.addrs) }
