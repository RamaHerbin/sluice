// Package placeholders implements a minimal, explicit set of request-time
// variables inspired by Caddy, such as {http.request.host} or {k8s.service}.
//
// Templates are parsed once at configuration compile time, so an unknown
// variable is a configuration error and never a request-time surprise. They
// are evaluated by a plain concatenation loop: no reflection, no text/template.
package placeholders

import (
	"errors"
	"fmt"
	"net/http"
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
// variable references. The zero value is an empty template.
type Template struct {
	segs []segment
}

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
	// TODO(wave-2, placeholders agent): implement.
	return Template{}, errors.New("placeholders: not implemented")
}

// Evaluate renders the template against c by concatenating its segments.
// Missing values render as "". CR and LF characters are removed from the
// result so a value can never inject a header line, and a result longer than
// MaxValueLength is dropped (Evaluate returns ""). Results are never
// re-parsed. c.Request must be the inbound request as received by the proxy,
// never the rewritten outbound request.
func (t Template) Evaluate(c Context) string {
	// TODO(wave-2, placeholders agent): implement.
	return ""
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

// Known reports whether name is an allowed variable: one of the fixed names
// above, or HeaderPrefix followed by a non-empty RFC 9110 token.
func Known(name string) bool {
	// TODO(wave-2, placeholders agent): implement.
	return false
}

// ValidateHeaderName reports whether name may be set by a header rule. It must
// be a non-empty RFC 9110 token of at most model.MaxHeaderNameLength bytes and
// must not be (case-insensitively) Host, Content-Length, Transfer-Encoding,
// Connection, Keep-Alive, Proxy-Authenticate, Proxy-Authorization,
// Proxy-Connection, TE, Trailer or Upgrade. X-Forwarded-* and X-Request-ID
// stay settable on purpose (see docs/design/placeholders.md).
func ValidateHeaderName(name string) error {
	// TODO(wave-2, placeholders agent): implement.
	return errors.New("placeholders: not implemented")
}
