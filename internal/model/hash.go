package model

import (
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
)

// canonicalWriter feeds a Snapshot into an FNV-64a hash as an unambiguous
// byte stream.
//
// Encoding rules, chosen so that no two distinct snapshots share a stream:
//   - every slice is preceded by its length, so an empty slice and a nil
//     slice are identical and the boundary between slices is explicit;
//   - every string is preceded by its byte length, so "ab" followed by "c"
//     never collides with "a" followed by "bc";
//   - integers are written as fixed-width big-endian values;
//   - booleans are one byte;
//   - times are written as Unix seconds plus nanoseconds in UTC, which
//     ignores the location and the monotonic reading.
//
// Field order is the declaration order of the model types. Hashing is not a
// wire format: nothing reads the stream back, so the encoding may change
// freely between releases as long as it stays canonical.
type canonicalWriter struct {
	h hash.Hash64
}

func (w *canonicalWriter) init() {
	if w.h == nil {
		w.h = fnv.New64a()
	}
}

func (w *canonicalWriter) sum() string {
	w.init()
	return fmt.Sprintf("%016x", w.h.Sum64())
}

func (w *canonicalWriter) writeSnapshot(s *Snapshot) {
	w.init()
	w.writeInt(len(s.Routes))
	for i := range s.Routes {
		w.writeRoute(&s.Routes[i])
	}
	w.writeInt(len(s.Backends))
	for i := range s.Backends {
		w.writeBackend(&s.Backends[i])
	}
}

func (w *canonicalWriter) writeRoute(r *Route) {
	w.writeString(r.Key.Namespace)
	w.writeString(r.Key.Name)
	w.writeInt(r.Key.Rule)
	w.writeInt(r.Key.Path)
	w.writeString(r.Host)
	w.writeString(r.Path)
	w.writeByte(byte(r.PathType))
	w.writeBackendKey(r.Backend)
	w.writeHeaderRules(r.RequestHeaders)
	w.writeHeaderRules(r.ResponseHeaders)
	utc := r.Created.UTC()
	w.writeInt64(utc.Unix())
	w.writeInt(utc.Nanosecond())
}

func (w *canonicalWriter) writeBackend(b *Backend) {
	w.writeBackendKey(b.Key)
	w.writeInt(len(b.Endpoints))
	for _, e := range b.Endpoints {
		w.writeString(e.Address)
		w.writeBool(e.Ready)
	}
}

func (w *canonicalWriter) writeBackendKey(k BackendKey) {
	w.writeString(k.Namespace)
	w.writeString(k.Service)
	w.writeInt64(int64(k.Port))
}

func (w *canonicalWriter) writeHeaderRules(rules []HeaderRule) {
	w.writeInt(len(rules))
	for _, r := range rules {
		w.writeString(r.Name)
		w.writeString(r.Value)
	}
}

func (w *canonicalWriter) writeString(s string) {
	w.writeInt(len(s))
	// hash.Hash.Write never returns an error.
	_, _ = w.h.Write([]byte(s))
}

func (w *canonicalWriter) writeInt(v int) {
	w.writeInt64(int64(v))
}

func (w *canonicalWriter) writeInt64(v int64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	_, _ = w.h.Write(buf[:])
}

func (w *canonicalWriter) writeBool(b bool) {
	if b {
		w.writeByte(1)
		return
	}
	w.writeByte(0)
}

func (w *canonicalWriter) writeByte(b byte) {
	_, _ = w.h.Write([]byte{b})
}
