package placeholders

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RamaHerbin/sluice/internal/model"
)

// Row numbers in comments and table fields refer to the test matrix in
// docs/design/placeholders.md section 7.

// Scenario 12 (Parse level): an unknown placeholder is a validation error at
// configuration time, reported through *UnknownVariableError with the name and
// nothing else from the source.
func TestScenario12UnknownPlaceholderIsParseError(t *testing.T) {
	_, err := Parse("route={nope}")
	if err == nil {
		t.Fatal("Parse accepted an unknown placeholder")
	}
	var uv *UnknownVariableError
	if !errors.As(err, &uv) {
		t.Fatalf("Parse error = %T %v, want *UnknownVariableError", err, err)
	}
	if uv.Name != "nope" {
		t.Errorf("UnknownVariableError.Name = %q, want %q", uv.Name, "nope")
	}
	if got, want := err.Error(), "unknown placeholder {nope}"; got != want {
		t.Errorf("error message = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "route=") {
		t.Errorf("error message %q echoes the template source", err.Error())
	}
}

func TestParse(t *testing.T) {
	type errKind int
	const (
		errNone errKind = iota
		errUnknown
		errUnbalanced
		errTooLong
	)
	tests := []struct {
		row        int
		name       string
		src        string
		wantSegs   int
		wantStatic bool
		wantErr    errKind
		wantName   string // errUnknown: UnknownVariableError.Name
		wantByte   int    // errUnbalanced: byte offset named in the message
	}{
		{row: 1, name: "empty source", src: "", wantSegs: 0, wantStatic: true},
		{row: 2, name: "literal only", src: "hello", wantSegs: 1, wantStatic: true},
		{row: 3, name: "fixed variable", src: "{http.request.host}", wantSegs: 1},
		{row: 4, name: "header variable", src: "{http.request.header.User-Agent}", wantSegs: 1},
		{row: 5, name: "header variable lowercase", src: "{http.request.header.user-agent}", wantSegs: 1},
		{row: 6, name: "header variable custom", src: "{http.request.header.X-Foo}", wantSegs: 1},
		{row: 7, name: "header variable empty name", src: "{http.request.header.}", wantErr: errUnknown, wantName: "http.request.header."},
		{row: 8, name: "header variable with space", src: "{http.request.header.X Foo}", wantErr: errUnknown, wantName: "http.request.header.X Foo"},
		{row: 9, name: "unknown name", src: "{unknown}", wantErr: errUnknown, wantName: "unknown"},
		{row: 10, name: "empty name", src: "{}", wantErr: errUnknown, wantName: ""},
		{row: 11, name: "lone open brace", src: "{", wantErr: errUnbalanced, wantByte: 0},
		{row: 12, name: "lone close brace", src: "}", wantErr: errUnbalanced, wantByte: 0},
		{row: 13, name: "doubled braces", src: "a{{b}}c", wantErr: errUnbalanced, wantByte: 2},
		{row: 14, name: "nested braces", src: "{a{b}}", wantErr: errUnbalanced, wantByte: 2},
		{row: 15, name: "never closed", src: "x{http.request.host", wantErr: errUnbalanced, wantByte: 1},
		{row: 16, name: "spaces inside braces", src: "{ http.request.host }", wantErr: errUnknown, wantName: " http.request.host "},
		{row: 17, name: "uppercase fixed name", src: "{HTTP.REQUEST.HOST}", wantErr: errUnknown, wantName: "HTTP.REQUEST.HOST"},
		{row: 18, name: "three variables two literals", src: "{http.request.remote}:{http.request.method} {http.request.path}", wantSegs: 5},
		{row: 19, name: "two variables one literal", src: "{k8s.namespace}/{k8s.service}", wantSegs: 3},
		{row: 20, name: "exactly the cap", src: strings.Repeat("a", MaxTemplateLength), wantSegs: 1, wantStatic: true},
		{row: 21, name: "one byte over the cap", src: strings.Repeat("a", MaxTemplateLength+1), wantErr: errTooLong},
		{row: 22, name: "valid variables over the cap", src: strings.Repeat("{k8s.route}", 373), wantErr: errTooLong},
		// Extra rows.
		{name: "close brace after literal", src: "a}b", wantErr: errUnbalanced, wantByte: 1},
		{name: "variable between literals", src: "x{http.request.host}y", wantSegs: 3},
		{name: "adjacent variables", src: "{k8s.route}{k8s.route}", wantSegs: 2},
		{name: "header variable never closed", src: "{http.request.header.X-Foo", wantErr: errUnbalanced, wantByte: 0},
		{name: "second reference unknown", src: "{k8s.route} {k8s.nope}", wantErr: errUnknown, wantName: "k8s.nope"},
		{name: "literal with control bytes", src: "a\r\nb", wantSegs: 1, wantStatic: true},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("row%02d %s", tc.row, tc.name), func(t *testing.T) {
			tmpl, err := Parse(tc.src)
			switch tc.wantErr {
			case errNone:
				if err != nil {
					t.Fatalf("Parse() error = %v, want nil", err)
				}
				if got := len(tmpl.segs); got != tc.wantSegs {
					t.Errorf("len(segs) = %d, want %d", got, tc.wantSegs)
				}
				if got := tmpl.IsStatic(); got != tc.wantStatic {
					t.Errorf("IsStatic() = %v, want %v", got, tc.wantStatic)
				}
			case errUnknown:
				var uv *UnknownVariableError
				if !errors.As(err, &uv) {
					t.Fatalf("Parse() error = %T %v, want *UnknownVariableError", err, err)
				}
				if uv.Name != tc.wantName {
					t.Errorf("Name = %q, want %q", uv.Name, tc.wantName)
				}
				if want := fmt.Sprintf("unknown placeholder {%s}", tc.wantName); err.Error() != want {
					t.Errorf("message = %q, want %q", err.Error(), want)
				}
			case errUnbalanced:
				if !errors.Is(err, ErrUnbalancedBraces) {
					t.Fatalf("Parse() error = %v, want ErrUnbalancedBraces", err)
				}
				if want := fmt.Sprintf("byte %d:", tc.wantByte); !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to contain %q", err.Error(), want)
				}
			case errTooLong:
				if !errors.Is(err, ErrTemplateTooLong) {
					t.Fatalf("Parse() error = %v, want ErrTemplateTooLong", err)
				}
				if want := fmt.Sprintf("%d bytes", len(tc.src)); !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to contain %q", err.Error(), want)
				}
			}
			if err != nil && tmpl.segs != nil {
				t.Errorf("Parse() returned a non-zero Template alongside an error")
			}
		})
	}
}

// Every Parse failure must leave the literal text out of the message: a
// static header value can be a secret.
func TestParseErrorsDoNotEchoSource(t *testing.T) {
	const secret = "s3cr3t-value"
	srcs := []string{
		secret + "{",
		secret + "}",
		secret + "{nope}",
		secret + "{k8s.route{",
		secret + strings.Repeat("a", MaxTemplateLength),
	}
	for _, src := range srcs {
		_, err := Parse(src)
		if err == nil {
			t.Errorf("Parse(%d-byte source) = nil error, want an error", len(src))
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q echoes the source literal", err.Error())
		}
	}
}

func TestIsStaticZeroTemplate(t *testing.T) {
	var zero Template
	if !zero.IsStatic() {
		t.Error("zero Template IsStatic() = false, want true")
	}
	if got := zero.Evaluate(Context{}); got != "" {
		t.Errorf("zero Template Evaluate() = %q, want \"\"", got)
	}
}

// newRequest builds an httptest request on target and applies mod to it.
func newRequest(target string, mod func(r *http.Request)) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if mod != nil {
		mod(r)
	}
	return r
}

func withHost(host string) func(r *http.Request) {
	return func(r *http.Request) { r.Host = host }
}

func withRemote(addr string) func(r *http.Request) {
	return func(r *http.Request) { r.RemoteAddr = addr }
}

func withHeader(name string, values ...string) func(r *http.Request) {
	return func(r *http.Request) {
		for _, v := range values {
			r.Header.Add(name, v)
		}
	}
}

// mustParse fails the test when src does not parse.
func mustParse(t *testing.T, src string) Template {
	t.Helper()
	tmpl, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%d-byte source) error = %v", len(src), err)
	}
	return tmpl
}

func TestEvaluate(t *testing.T) {
	// Row 37 in the design doc says 200 repetitions, but 200 * 23 bytes
	// exceeds MaxTemplateLength. 178 repetitions (4094 bytes) is the largest
	// template that parses; 178 * 100 bytes is still far over MaxValueLength.
	amplify := strings.Repeat("{"+HeaderPrefix+"A}", MaxTemplateLength/len("{"+HeaderPrefix+"A}"))

	tests := []struct {
		row  int
		name string
		tmpl string
		ctx  Context
		want string
	}{
		{row: 23, name: "host strips port keeps case", tmpl: "{http.request.host}",
			ctx: Context{Request: newRequest("/", withHost("Example.com:8080"))}, want: "Example.com"},
		{row: 24, name: "host keeps IPv6 brackets", tmpl: "{http.request.host}",
			ctx: Context{Request: newRequest("/", withHost("[::1]:8080"))}, want: "[::1]"},
		{row: 25, name: "host without port unchanged", tmpl: "{http.request.host}",
			ctx: Context{Request: newRequest("/", withHost("example.com"))}, want: "example.com"},
		{name: "host IPv6 without port unchanged", tmpl: "{http.request.host}",
			ctx: Context{Request: newRequest("/", withHost("[::1]"))}, want: "[::1]"},
		{name: "host empty", tmpl: "{http.request.host}",
			ctx: Context{Request: newRequest("/", withHost(""))}, want: ""},
		{name: "method", tmpl: "{http.request.method}",
			ctx: Context{Request: httptest.NewRequest(http.MethodPost, "/", nil)}, want: "POST"},
		{row: 26, name: "remote IPv4", tmpl: "{http.request.remote}",
			ctx: Context{Request: newRequest("/", withRemote("10.0.0.1:34567"))}, want: "10.0.0.1"},
		{row: 27, name: "remote IPv6", tmpl: "{http.request.remote}",
			ctx: Context{Request: newRequest("/", withRemote("[::1]:5000"))}, want: "::1"},
		{name: "remote IPv6 with zone", tmpl: "{http.request.remote}",
			ctx: Context{Request: newRequest("/", withRemote("[fe80::1%eth0]:5000"))}, want: "fe80::1%eth0"},
		{row: 28, name: "remote raw fallback", tmpl: "{http.request.remote}",
			ctx: Context{Request: newRequest("/", withRemote("bogus"))}, want: "bogus"},
		{name: "remote empty", tmpl: "{http.request.remote}",
			ctx: Context{Request: newRequest("/", withRemote(""))}, want: ""},
		{row: 29, name: "path keeps escapes", tmpl: "{http.request.path}",
			ctx: Context{Request: newRequest("/a%20b/%2F", nil)}, want: "/a%20b/%2F"},
		{row: 30, name: "path keeps encoded CRLF", tmpl: "{http.request.path}",
			ctx: Context{Request: newRequest("/a%0D%0Ab", nil)}, want: "/a%0D%0Ab"},
		{name: "path drops query string", tmpl: "{http.request.path}",
			ctx: Context{Request: newRequest("/x?y=1", nil)}, want: "/x"},
		{name: "path with nil URL", tmpl: "{http.request.path}",
			ctx: Context{Request: &http.Request{}}, want: ""},
		{row: 31, name: "header repeated lines joined", tmpl: "{http.request.header.X-Multi}",
			ctx: Context{Request: newRequest("/", withHeader("X-Multi", "a", "b"))}, want: "a, b"},
		{row: 32, name: "header missing", tmpl: "{http.request.header.X-Missing}",
			ctx: Context{Request: newRequest("/", nil)}, want: ""},
		{name: "header present but empty", tmpl: "{http.request.header.X-Empty}",
			ctx: Context{Request: newRequest("/", withHeader("X-Empty", ""))}, want: ""},
		{row: 33, name: "header CRLF stripped", tmpl: "{http.request.header.X-Evil}",
			ctx: Context{Request: newRequest("/", func(r *http.Request) {
				r.Header["X-Evil"] = []string{"a\r\nInjected: yes"}
			})}, want: "aInjected: yes"},
		{row: 4, name: "header canonical name", tmpl: "{http.request.header.User-Agent}",
			ctx: Context{Request: newRequest("/", withHeader("User-Agent", "sluice-test"))}, want: "sluice-test"},
		{row: 5, name: "header lowercase name reads the same field", tmpl: "{http.request.header.user-agent}",
			ctx: Context{Request: newRequest("/", withHeader("User-Agent", "sluice-test"))}, want: "sluice-test"},
		{row: 34, name: "nil request host", tmpl: "{http.request.host}",
			ctx: Context{}, want: ""},
		{name: "nil request header", tmpl: "{http.request.header.X-Foo}",
			ctx: Context{}, want: ""},
		{name: "nil request does not hide k8s values", tmpl: "{k8s.service}",
			ctx: Context{Service: "svc"}, want: "svc"},
		{row: 35, name: "upstream address missing", tmpl: "{upstream.address}",
			ctx: Context{UpstreamAddress: ""}, want: ""},
		{name: "upstream address", tmpl: "{upstream.address}",
			ctx: Context{UpstreamAddress: "10.1.2.3:8080"}, want: "10.1.2.3:8080"},
		{name: "namespace", tmpl: "{k8s.namespace}", ctx: Context{Namespace: "ns"}, want: "ns"},
		{name: "route", tmpl: "{k8s.route}", ctx: Context{Route: "ns/ing"}, want: "ns/ing"},
		{name: "namespace and service", tmpl: "{k8s.namespace}/{k8s.service}",
			ctx: Context{Namespace: "ns", Service: "svc"}, want: "ns/svc"},
		{name: "missing value inside literals", tmpl: "{k8s.namespace}/{k8s.service}",
			ctx: Context{Namespace: "ns"}, want: "ns/"},
		{name: "literals only", tmpl: "https", ctx: Context{}, want: "https"},
		{name: "literal CRLF stripped", tmpl: "a\rb\nc", ctx: Context{}, want: "abc"},
		{row: 36, name: "no recursion", tmpl: "{http.request.header.X}",
			ctx: Context{Request: newRequest("/", withHeader("X", "{k8s.service}")), Service: "svc"}, want: "{k8s.service}"},
		{row: 37, name: "amplified value dropped", tmpl: amplify,
			ctx: Context{Request: newRequest("/", withHeader("A", strings.Repeat("v", 100)))}, want: ""},
		{name: "value exactly at the cap kept", tmpl: "{http.request.header.A}",
			ctx:  Context{Request: newRequest("/", withHeader("A", strings.Repeat("v", MaxValueLength)))},
			want: strings.Repeat("v", MaxValueLength)},
		{name: "value one byte over the cap dropped", tmpl: "{http.request.header.A}",
			ctx: Context{Request: newRequest("/", withHeader("A", strings.Repeat("v", MaxValueLength+1)))}, want: ""},
		{name: "concatenation one byte over the cap dropped", tmpl: "{http.request.header.A}x",
			ctx: Context{Request: newRequest("/", withHeader("A", strings.Repeat("v", MaxValueLength)))}, want: ""},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("row%02d %s", tc.row, tc.name), func(t *testing.T) {
			tmpl := mustParse(t, tc.tmpl)
			got := tmpl.Evaluate(tc.ctx)
			if got != tc.want {
				t.Errorf("Evaluate() = %q (%d bytes), want %q (%d bytes)", trim(got), len(got), trim(tc.want), len(tc.want))
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("Evaluate() result contains CR or LF")
			}
			if len(got) > MaxValueLength {
				t.Errorf("Evaluate() result is %d bytes, over MaxValueLength", len(got))
			}
		})
	}
}

// trim shortens long strings for failure messages.
func trim(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// Scenario 11: every allowed placeholder is evaluated against a real request
// and the Context the proxy would build. One template references them all so
// the test also covers concatenation.
func TestScenario11AllowedPlaceholdersEvaluated(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/items%20x?q=1", nil)
	req.Host = "Shop.Example.com:30080"
	req.RemoteAddr = "192.0.2.7:51234"
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Add("Accept", "text/html")
	req.Header.Add("Accept", "application/json")

	ctx := Context{
		Request:         req,
		Namespace:       "shop",
		Service:         "catalog",
		Route:           "shop/catalog-ingress",
		UpstreamAddress: "10.244.1.5:8080",
	}

	tmpl := mustParse(t, "{http.request.remote} {http.request.method} {http.request.host}{http.request.path}"+
		" ua={http.request.header.User-Agent} accept={http.request.header.accept}"+
		" {k8s.namespace}/{k8s.service} {k8s.route} -> {upstream.address}")
	want := "192.0.2.7 PUT Shop.Example.com/api/v1/items%20x" +
		" ua=curl/8.0 accept=text/html, application/json" +
		" shop/catalog shop/catalog-ingress -> 10.244.1.5:8080"
	if got := tmpl.Evaluate(ctx); got != want {
		t.Errorf("Evaluate() =\n  %q\nwant\n  %q", got, want)
	}
	if tmpl.IsStatic() {
		t.Error("IsStatic() = true for a template full of variables")
	}
}

func TestKnown(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{VarRequestHost, true},
		{VarRequestMethod, true},
		{VarRequestPath, true},
		{VarRequestRemote, true},
		{VarNamespace, true},
		{VarService, true},
		{VarRoute, true},
		{VarUpstreamAddress, true},
		{HeaderPrefix + "User-Agent", true},
		{HeaderPrefix + "user-agent", true},
		{HeaderPrefix + "X-Foo", true},
		{HeaderPrefix + "!#$%&'*+-.^_`|~09AZaz", true}, // every tchar class
		{HeaderPrefix, false},                          // empty header name
		{HeaderPrefix + "X Foo", false},                // space
		{HeaderPrefix + "X-Foo:", false},               // colon
		{HeaderPrefix + "X-Foo\r", false},              // control byte
		{HeaderPrefix + "caf\xc3\xa9", false},          // non-ASCII
		{"http.request.headerX-Foo", false},            // missing dot
		{"", false},
		{"unknown", false},
		{"HTTP.REQUEST.HOST", false},
		{" " + VarRequestHost, false},
		{VarRequestHost + " ", false},
		{"http.request", false},
		{"k8s", false},
		{"env.HOME", false},
	}
	for _, tc := range tests {
		if got := Known(tc.name); got != tc.want {
			t.Errorf("Known(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidateHeaderName(t *testing.T) {
	tests := []struct {
		row     int
		name    string
		wantErr error // nil for ok; otherwise matched with errors.Is
	}{
		{row: 38, name: "X-Foo"},
		{row: 39, name: "x-foo"},
		{row: 40, name: "X Foo", wantErr: errHeaderNameNotToken},
		{row: 41, name: "", wantErr: errHeaderNameEmpty},
		{row: 42, name: "X-Foo:", wantErr: errHeaderNameNotToken},
		{row: 43, name: "Host", wantErr: errHeaderNameDenied},
		{row: 43, name: "host", wantErr: errHeaderNameDenied},
		{row: 43, name: "HOST", wantErr: errHeaderNameDenied},
		{row: 44, name: "Content-Length", wantErr: errHeaderNameDenied},
		{row: 44, name: "TE", wantErr: errHeaderNameDenied},
		{row: 44, name: "Proxy-Connection", wantErr: errHeaderNameDenied},
		{row: 45, name: "X-Forwarded-Proto"},
		{name: "X-Forwarded-For"},
		{name: "X-Forwarded-Host"},
		{name: "Forwarded"},
		{name: "X-Request-ID"},
		{row: 46, name: strings.Repeat("a", model.MaxHeaderNameLength+1), wantErr: errHeaderNameTooLong},
		{name: strings.Repeat("a", model.MaxHeaderNameLength)},
		{name: "X-Foo\r", wantErr: errHeaderNameNotToken},
		{name: "caf\xc3\xa9", wantErr: errHeaderNameNotToken},
		{name: "# note", wantErr: errHeaderNameNotToken},
		{name: "Hosts"},       // not on the list: prefix match must not deny
		{name: "X-Host"},      // same
		{name: "Connections"}, // same
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("row%02d %s", tc.row, trim(tc.name)), func(t *testing.T) {
			err := ValidateHeaderName(tc.name)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateHeaderName() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateHeaderName() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// The whole deny-list is rejected in lower, upper and canonical case.
func TestValidateHeaderNameDenyList(t *testing.T) {
	for _, name := range deniedHeaderNames {
		for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			if err := ValidateHeaderName(variant); !errors.Is(err, errHeaderNameDenied) {
				t.Errorf("ValidateHeaderName(%q) error = %v, want denied", variant, err)
			}
		}
	}
}

// A malformed name and a denied name must be told apart by their messages, so
// a Warning reason explains which rule was broken.
func TestValidateHeaderNameDistinctMessages(t *testing.T) {
	notToken := ValidateHeaderName("X Foo")
	denied := ValidateHeaderName("Host")
	tooLong := ValidateHeaderName(strings.Repeat("a", model.MaxHeaderNameLength+1))
	empty := ValidateHeaderName("")
	msgs := map[string]string{
		"not a token": notToken.Error(),
		"denied":      denied.Error(),
		"too long":    tooLong.Error(),
		"empty":       empty.Error(),
	}
	seen := map[string]string{}
	for kind, msg := range msgs {
		if msg == "" {
			t.Errorf("%s: empty message", kind)
		}
		if other, dup := seen[msg]; dup {
			t.Errorf("%s and %s share the message %q", kind, other, msg)
		}
		seen[msg] = kind
	}
	if !strings.Contains(tooLong.Error(), fmt.Sprintf("max %d", model.MaxHeaderNameLength)) {
		t.Errorf("too long message %q does not name the limit", tooLong.Error())
	}
}

func BenchmarkEvaluate(b *testing.B) {
	tmpl, err := Parse("{http.request.remote} {http.request.method} {http.request.path} ua={http.request.header.User-Agent} {k8s.route}")
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/items?x=1", nil)
	req.Header.Set("User-Agent", "bench/1.0")
	c := Context{Request: req, Namespace: "ns", Service: "svc", Route: "ns/ing", UpstreamAddress: "10.0.0.2:8080"}
	b.ReportAllocs()
	for b.Loop() {
		if tmpl.Evaluate(c) == "" {
			b.Fatal("empty result")
		}
	}
}

func BenchmarkEvaluateStatic(b *testing.B) {
	tmpl, err := Parse("https")
	if err != nil {
		b.Fatal(err)
	}
	c := Context{Request: httptest.NewRequest(http.MethodGet, "/", nil)}
	b.ReportAllocs()
	for b.Loop() {
		if tmpl.Evaluate(c) != "https" {
			b.Fatal("unexpected result")
		}
	}
}
