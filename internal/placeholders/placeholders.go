// Package placeholders implements a minimal, explicit set of request-time
// variables inspired by Caddy, such as {http.request.host} or {k8s.service}.
//
// Templates are parsed once at configuration compile time, so an unknown
// variable is a configuration error and never a request-time surprise. They
// are evaluated by a plain concatenation loop: no reflection, no text/template.
//
// The specification, including the reasoning behind each variable and the
// security limits, lives in docs/design/placeholders.md.
package placeholders

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/RamaHerbin/sluice/internal/model"
)

// HeaderPrefix is the prefix of the open-ended variable family
// {http.request.header.<Name>}.
const HeaderPrefix = "http.request.header."

// Fixed variable names. Header variables are matched by HeaderPrefix.
const (
	VarRequestHost     = "http.request.host"
	VarRequestMethod   = "http.request.method"
	VarRequestPath     = "http.request.path"
	VarRequestRemote   = "http.request.remote" // client IP without port
	VarNamespace       = "k8s.namespace"
	VarService         = "k8s.service"
	VarRoute           = "k8s.route" // "namespace/name" of the source Ingress
	VarUpstreamAddress = "upstream.address"
)

// MaxTemplateLength bounds the size of a template source string in bytes.
const MaxTemplateLength = 4096

// MaxValueLength bounds the size of an evaluated value in bytes. Longer
// results are dropped: Evaluate returns "" and the header is not set.
const MaxValueLength = 8192

// Context carries the request-time values a template can read. Any field may
// be zero; missing values evaluate to the empty string.
type Context struct {
	Request         *http.Request
	Namespace       string
	Service         string
	Route           string
	UpstreamAddress string
}

// Template is a parsed placeholder string made of literal segments and
// variable references. The zero value is an empty template. A Template holds
// no mutable state, so one value may be evaluated concurrently.
type Template struct {
	segs []segment
}

// segment is one piece of a template: either a run of literal bytes or a
// single variable reference, never both.
type segment struct {
	lit      string
	variable string // empty for a literal segment
}

// UnknownVariableError reports a {name} that is not in the registry.
type UnknownVariableError struct {
	Name string
}

func (e *UnknownVariableError) Error() string {
	return fmt.Sprintf("unknown placeholder {%s}", e.Name)
}

// ErrUnbalancedBraces is returned when a template has a "{" without a
// matching "}" or vice versa. There is no escape syntax: braces are always
// structural.
var ErrUnbalancedBraces = errors.New("unbalanced braces in placeholder template")

// ErrTemplateTooLong is returned when a template source exceeds
// MaxTemplateLength.
var ErrTemplateTooLong = errors.New("placeholder template exceeds MaxTemplateLength")

// Parse compiles src into a Template. Unknown variables (including empty
// names and header names that are not RFC 9110 tokens) are reported as
// *UnknownVariableError, unbalanced or nested braces as ErrUnbalancedBraces,
// and sources longer than MaxTemplateLength as ErrTemplateTooLong. Error
// messages never echo the template source, which may hold secrets.
func Parse(src string) (Template, error) {
	// The length check runs before any scanning so an oversized source costs
	// nothing beyond len().
	if len(src) > MaxTemplateLength {
		return Template{}, fmt.Errorf("template is %d bytes, max %d: %w", len(src), MaxTemplateLength, ErrTemplateTooLong)
	}

	var segs []segment
	start := 0 // first byte of the literal currently being scanned
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '}':
			// A closing brace outside a reference.
			return Template{}, fmt.Errorf("byte %d: %w", i, ErrUnbalancedBraces)
		case '{':
			if i > start {
				segs = append(segs, segment{lit: src[start:i]})
			}
			// The reference ends at the next brace, which must be a "}".
			// A second "{" first means nesting; no brace at all means the
			// reference was never closed.
			end := strings.IndexAny(src[i+1:], "{}")
			if end < 0 {
				return Template{}, fmt.Errorf("byte %d: %w", i, ErrUnbalancedBraces)
			}
			end += i + 1
			if src[end] == '{' {
				return Template{}, fmt.Errorf("byte %d: %w", end, ErrUnbalancedBraces)
			}
			name := src[i+1 : end]
			if !Known(name) {
				return Template{}, &UnknownVariableError{Name: name}
			}
			segs = append(segs, segment{variable: name})
			i = end
			start = end + 1
		}
	}
	if start < len(src) {
		segs = append(segs, segment{lit: src[start:]})
	}
	return Template{segs: segs}, nil
}

// Evaluate renders the template against c by concatenating its segments.
// Missing values render as "". CR and LF characters are removed from the
// result so a value can never inject a header line, and a result longer than
// MaxValueLength is dropped (Evaluate returns ""). Results are never
// re-parsed. c.Request must be the inbound request as received by the proxy,
// never the rewritten outbound request.
func (t Template) Evaluate(c Context) string {
	switch len(t.segs) {
	case 0:
		return ""
	case 1:
		// A static value or a lone variable, the common shapes. No builder
		// is needed because there is nothing to concatenate.
		v := stripCRLF(t.segs[0].render(c))
		if len(v) > MaxValueLength {
			return ""
		}
		return v
	}

	n := 0
	for _, s := range t.segs {
		n += len(s.lit)
	}
	var b strings.Builder
	b.Grow(n)
	for _, s := range t.segs {
		v := stripCRLF(s.render(c))
		// Stop as soon as the cap is exceeded instead of building the whole
		// value: the cap exists to bound amplification.
		if b.Len()+len(v) > MaxValueLength {
			return ""
		}
		b.WriteString(v)
	}
	return b.String()
}

// IsStatic reports whether the template contains no variable.
func (t Template) IsStatic() bool {
	for _, s := range t.segs {
		if s.variable != "" {
			return false
		}
	}
	return true
}

// render returns the segment's literal text or the value of its variable.
func (s segment) render(c Context) string {
	if s.variable == "" {
		return s.lit
	}
	return lookup(s.variable, c)
}

// lookup returns the value of one registered variable, or "" when the value
// is missing. Names reach it only through Parse, so an unknown name here is
// impossible in practice and renders as "" rather than failing.
func lookup(name string, c Context) string {
	switch name {
	case VarNamespace:
		return c.Namespace
	case VarService:
		return c.Service
	case VarRoute:
		return c.Route
	case VarUpstreamAddress:
		return c.UpstreamAddress
	}

	// Everything below reads the inbound request.
	r := c.Request
	if r == nil {
		return ""
	}
	switch name {
	case VarRequestHost:
		return hostOnly(r.Host)
	case VarRequestMethod:
		return r.Method
	case VarRequestPath:
		if r.URL == nil {
			return ""
		}
		// The wire form, never the decoded URL.Path: a decoded path can
		// carry CR, LF or other control bytes that the transport rejects.
		return r.URL.EscapedPath()
	case VarRequestRemote:
		return remoteIP(r.RemoteAddr)
	}
	if rest, ok := strings.CutPrefix(name, HeaderPrefix); ok {
		// Header.Values canonicalizes the key, so user-agent and User-Agent
		// read the same field. Repeated lines are joined in RFC 9110 list
		// form. Values on a nil map is a nil slice, so a request without
		// headers is fine.
		return strings.Join(r.Header.Values(rest), ", ")
	}
	return ""
}

// hostOnly strips the port from a Host header value: the value is cut at the
// last ":" when that colon comes after the last "]". IPv6 brackets and case
// are kept. net.SplitHostPort is not used because it drops the brackets and
// fails on a value without a port.
func hostOnly(host string) string {
	colon := strings.LastIndexByte(host, ':')
	if colon < 0 || colon < strings.LastIndexByte(host, ']') {
		return host
	}
	return host[:colon]
}

// remoteIP returns the host part of a RemoteAddr, or the raw value when it is
// not a host:port pair. RemoteAddr comes from the listener, never from the
// client, so a value that does not split means a non-TCP listener or a test.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// stripCRLF removes every CR and LF byte from s. The common case, no such
// byte at all, returns s without allocating.
func stripCRLF(s string) string {
	if strings.IndexAny(s, "\r\n") < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\r' && c != '\n' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Known reports whether name is an allowed variable: one of the fixed names
// above, or HeaderPrefix followed by a non-empty RFC 9110 token.
func Known(name string) bool {
	switch name {
	case VarRequestHost, VarRequestMethod, VarRequestPath, VarRequestRemote,
		VarNamespace, VarService, VarRoute, VarUpstreamAddress:
		return true
	}
	if rest, ok := strings.CutPrefix(name, HeaderPrefix); ok {
		return isToken(rest)
	}
	return false
}

// Errors returned by ValidateHeaderName. They stay unexported: callers wrap
// the error with the header name and report it, they never branch on it.
var (
	errHeaderNameEmpty    = errors.New("header name is empty")
	errHeaderNameNotToken = errors.New("header name is not an RFC 9110 token")
	errHeaderNameTooLong  = errors.New("header name exceeds the maximum length")
	errHeaderNameDenied   = errors.New("header name is reserved: framing and hop-by-hop headers cannot be set by a rule")
)

// deniedHeaderNames lists the headers a rule may never set, compared
// case-insensitively. Host is written from the outbound request's Host field,
// Content-Length and Transfer-Encoding frame the message, and the rest are
// hop-by-hop headers that httputil.ReverseProxy strips (Proxy-Connection is a
// non-standard one it also treats as hop-by-hop). X-Forwarded-*, Forwarded
// and X-Request-ID are deliberately absent.
var deniedHeaderNames = []string{
	"Host",
	"Content-Length",
	"Transfer-Encoding",
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Upgrade",
}

// ValidateHeaderName reports whether name may be set by a header rule. It must
// be a non-empty RFC 9110 token of at most model.MaxHeaderNameLength bytes and
// must not be (case-insensitively) Host, Content-Length, Transfer-Encoding,
// Connection, Keep-Alive, Proxy-Authenticate, Proxy-Authorization,
// Proxy-Connection, TE, Trailer or Upgrade. X-Forwarded-* and X-Request-ID
// stay settable on purpose (see docs/design/placeholders.md).
func ValidateHeaderName(name string) error {
	if name == "" {
		return errHeaderNameEmpty
	}
	if !isToken(name) {
		return errHeaderNameNotToken
	}
	if len(name) > model.MaxHeaderNameLength {
		return fmt.Errorf("header name is %d bytes, max %d: %w", len(name), model.MaxHeaderNameLength, errHeaderNameTooLong)
	}
	for _, denied := range deniedHeaderNames {
		if strings.EqualFold(name, denied) {
			return errHeaderNameDenied
		}
	}
	return nil
}

// isToken reports whether s is a non-empty RFC 9110 token (section 5.6.2).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTchar(s[i]) {
			return false
		}
	}
	return true
}

// isTchar reports whether c is an RFC 9110 tchar:
// "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." / "^" / "_" /
// "`" / "|" / "~" / DIGIT / ALPHA.
func isTchar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
