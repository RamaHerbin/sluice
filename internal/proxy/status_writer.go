package proxy

import "net/http"

// statusWriter captures the final status code sent to the client so the
// access log and metrics can report it. It embeds the ResponseWriter, which
// promotes only Header, Write and WriteHeader; Unwrap lets
// http.ResponseController reach the real Flusher and Hijacker underneath,
// which is what keeps streaming and protocol upgrades working through
// ReverseProxy.
type statusWriter struct {
	http.ResponseWriter
	// status is the first final status code written, 0 until then.
	status int
}

// WriteHeader records the first final status and forwards every call.
// Informational codes (ReverseProxy forwards 103 Early Hints before the real
// response) are forwarded but not recorded, except 101, which ends the HTTP
// exchange by switching protocols. Only the first final code counts, as in
// net/http, where later calls are superfluous.
//
// A 1xx call can arrive from the transport's read goroutine while the
// handler goroutine waits in RoundTrip; that path touches no field of the
// wrapper.
func (w *statusWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		if w.status == 0 {
			w.status = code
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write records the implicit 200 that net/http sends when a handler writes a
// body without calling WriteHeader first.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
