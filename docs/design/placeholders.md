# Placeholders

Status: v0, decided 2026-09-03. Implements the frozen contract in `internal/placeholders/placeholders.go`. Facts about Go behaviour below were verified against Go 1.26 with a scratch program (server and transport validation, `EscapedPath`, `SplitHostPort`).

Borrowed from Caddy (https://caddyserver.com/docs/conventions#placeholders, `modules/caddyhttp/replacer.go`): the `{dotted.name}` syntax, the `http.request.*` naming, port stripping on `{http.request.host}`, comma joining of repeated header lines, raw fallback when `RemoteAddr` does not split. Deliberately not borrowed: the backslash escape, the `{env.*}`, `{file.*}`, `{system.*}`, `{time.*}` families, unknown names rendered as empty at request time, decoded `{http.request.uri.path}`, and the append/delete header operators.

## 1. Grammar

```
template    = *( literal / reference ) ;
literal     = 1*( any byte except "{" and "}" ) ;
reference   = "{" name "}" ;
name        = fixed-name / header-name ;
fixed-name  = "http.request.host" / "http.request.method" / "http.request.path"
            / "http.request.remote" / "k8s.namespace" / "k8s.service"
            / "k8s.route" / "upstream.address" ;
header-name = "http.request.header." token ;
token       = 1*tchar ;                                   RFC 9110 section 5.6.2
tchar       = "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." / "^"
            / "_" / "`" / "|" / "~" / DIGIT / ALPHA ;
```

- Braces are structural, never literal. `{` opens a reference that ends at the next `}`. A second `{` before that `}`, an end of input before it, or a `}` outside a reference is `ErrUnbalancedBraces`.
- Nothing is trimmed inside braces and fixed names are case-sensitive: `{ http.request.host }` and `{HTTP.REQUEST.HOST}` are unknown variables. The header part is looked up canonically (section 2), so `{http.request.header.user-agent}` works.
- No escape syntax in the MVP. The argument "braces are not valid in header values" is false: RFC 9110 `field-vchar` covers all of VCHAR (0x21 to 0x7E), so `{` and `}` are legal and appear in JSON-shaped values. The real reasons: no Ingress use case for a literal brace in a header rule has been identified; a bare brace as an error makes typos loud at config time; and the natural future escape (`{{` for `{`, `}}` for `}`) is a parse error today, so adding it later breaks no valid template. Caddy's `\{` is not borrowed because a backslash is an ordinary literal here and reserving it would add a second special character.
- The empty source is a valid template: `Parse("")` returns the zero `Template`, which is static and evaluates to `""`.
- `len(src)` in bytes must not exceed `MaxTemplateLength` (4096); checked before scanning. Bytes rather than runes because the bound protects memory, not readability.
- Adjacent literals may be merged into one segment. A reference segment stores the name exactly as written.

## 2. Registry

`Known(name)` is the registry and nothing else consults a list: it returns true when `name` equals one of the eight constants, or when `strings.HasPrefix(name, HeaderPrefix)` and the remainder is a non-empty RFC 9110 token. Because token checking lives inside `Known`, `Parse` reports every rejected name, including `{}` and `{http.request.header.}`, as `*UnknownVariableError`.

| Variable | Go source (c is the `Context`) | Decision and reason |
|---|---|---|
| `{http.request.host}` | `hostOnly(c.Request.Host)`: cut at the last `:` when it comes after the last `]` | Port stripped, as Caddy does: the inbound port is sluice's own listener (a NodePort mapping), meaningless to a backend. `net.SplitHostPort` is not used because it drops IPv6 brackets and errors on a portless host; `hostOnly` gives `example.com:8080` -> `example.com`, `[::1]:8080` -> `[::1]`, `example.com` unchanged. Case is preserved: this is a value copy, not the lookup key (`router.Match` lowercases for matching). |
| `{http.request.method}` | `c.Request.Method` | Validated as a token by Go's server. |
| `{http.request.path}` | `c.Request.URL.EscapedPath()` | Not the decoded `URL.Path`: `/a%0D%0Ab` decodes to `/a\r\nb` and `/x%01y` to a control byte. Go's transport refuses such header values (`net/http: invalid header field value`), which `ReverseProxy` turns into a 502, so a client could break any route whose rule mentions the path. `EscapedPath` is the wire form the backend already sees in its request line and is always header-safe. Caddy's decoded path is deliberately not borrowed. The query string has no variable: it never reaches logs or headers. |
| `{http.request.remote}` | `host, _, err := net.SplitHostPort(c.Request.RemoteAddr)`; on error the raw `RemoteAddr` | The direct TCP peer without port (`10.0.0.1:34567` -> `10.0.0.1`, `[::1]:5000` -> `::1`, zone kept). Raw fallback follows Caddy: `RemoteAddr` comes from the listener, never from the client, so a malformed value means a non-TCP listener or a test, and losing it helps nobody. This is not the origin behind a load balancer: no trusted-proxy list in the MVP. |
| `{k8s.namespace}` | `c.Namespace` | `router.Route.Namespace`, the backend Service's namespace. |
| `{k8s.service}` | `c.Service` | `router.Route.Service`. |
| `{k8s.route}` | `c.Route` | `Key.Namespace + "/" + Key.Name` of the source Ingress, without the `#r0p1` suffix (contract comment). |
| `{upstream.address}` | `c.UpstreamAddress` | The `host:port` returned by `Picker.Pick`, carried in the request context so response rules see it too. |
| `{http.request.header.<Name>}` | `strings.Join(c.Request.Header.Values(rest), ", ")` | `Header.Values` canonicalizes through `textproto.CanonicalMIMEHeaderKey`, so `user-agent` and `User-Agent` read one field. Repeated field lines are joined with `", "`, the RFC 9110 section 5.3 list form and what the Fetch API's `Headers.get` shows developers; Caddy joins with a bare `,`. `Cookie` is the one header that must not be comma-joined (RFC 6265). Go's HTTP/2 server merges cookie crumbs with `"; "` into one line, but the HTTP/1.1 server keeps repeated `Cookie` lines separate, so `{http.request.header.Cookie}` would join them with `", "` there. The MVP does not special-case it; documented limitation. Absent header: `""`. |

Caller contract: `Context.Request` is always the inbound request (`pr.In` in `Rewrite`; for `ModifyResponse` the proxy keeps the inbound request in its per-request context state). Never `pr.Out`: request and response rules must read the same values, and `SetXForwarded` or earlier rules must not feed back into templates.

## 3. Evaluation semantics

1. Concatenation loop: a `strings.Builder` grown to the total literal length, then per segment either the literal or `lookup(name, c)`. `lookup` returns `""` when `c.Request` is nil (all `http.*` names), when the header is absent, or when the string field is empty. Rendering `""` for a missing value is the whole "copy if present" mechanism.
2. No recursion: an evaluated value is never re-parsed. A header containing `{k8s.service}` is forwarded as those literal characters.
3. Sanitization: every `\r` and `\n` is removed from the final string (a byte loop, no regexp). Every registered source is header-safe by construction (Go's server answers 400 to control bytes in header values or a bare CR, verified; `EscapedPath`; kernel-provided `RemoteAddr`; DNS labels), so this is defense in depth for a future variable or a differently validated path.
4. Cap: `MaxValueLength` = 8192 bytes. A result longer than that is dropped: `Evaluate` returns `""`, so the header is not set. Drop beats truncate because a cut value is corrupt data a backend may still act on (half a token, half a JSON document), while absence is a state backends already handle, and it reuses the empty rule instead of adding an API. The threat is amplification: a 4 KiB template can reference `{http.request.header.A}` about 170 times and turn one 48 KiB inbound header into 8 MiB outbound. 8 KiB matches nginx's `large_client_header_buffers` and Apache's `LimitRequestFieldSize`. Check the length after each append and stop early.
5. Set semantics: the proxy runs `if v := h.Value.Evaluate(ctx); v != "" { hdr.Set(h.Name, v) }` for each rule in annotation order. `Set` replaces client-supplied values of the same name (nginx `proxy_set_header` behaviour). `Header.Set` canonicalizes the wire name (`x-foo` -> `X-Foo`); HTTP/2 lowercases anyway.
6. `Evaluate` never fails, never logs, never allocates beyond the builder, and is O(len(template) + len(values)) with the early stop.

## 4. Header rules

```yaml
metadata:
  annotations:
    sluice.dev/request-headers: |
      X-Forwarded-Proto: https
      X-Sluice-Route: {k8s.route}
      X-Client-IP: {http.request.remote}
    sluice.dev/response-headers: |
      X-Served-By: {k8s.namespace}/{k8s.service}
```

Parsing happens in `controller.Translate`, the only reader of Kubernetes objects:

1. Split on `\n`, strip one trailing `\r` per line, `strings.TrimSpace` each line.
2. Skip empty lines. There is no comment syntax: `# note` fails as an invalid header name (space).
3. A line must contain `:`; split at the first one; `name = TrimSpace(left)`, `value = TrimSpace(right)`. A value may itself contain colons.
4. `name` goes through `ValidateHeaderName`. `value` must be non-empty, then goes through `Parse`. An empty value is rejected because a rule that can never fire is a mistake, and because `Name:` with nothing after it is the natural future syntax for "delete this header".
5. Duplicate names inside one annotation, compared with `strings.EqualFold`, are an error. The two annotations are independent: `X-A` may appear in both.
6. At most 32 rules per annotation. The value is bounded by `MaxTemplateLength`, the name by 256 bytes.
7. Any error skips the whole Ingress with a `Warning{Object: "ns/name", Reason}` and `IngressSkipped()`. `Reason` names the annotation, the line number, the header name and the error, never the value text (a static value can be a secret such as `X-Api-Key: ...`).

`ValidateHeaderName(name)` accepts a non-empty RFC 9110 token of at most 256 bytes that is not, case-insensitively, one of: `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Upgrade`, plus `Proxy-Connection`, which Go's `httputil` treats as hop-by-hop. Why a deny-list at all: `ReverseProxy` strips hop-by-hop headers before it calls `Rewrite`, so anything a rule sets reaches the transport untouched. Hand-set framing headers corrupt the exchange (a `Content-Length` mismatch, forged chunking) or are silently ignored (`Host` is written from `Out.Host`), and both are confusing rather than useful.

`X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto` and `Forwarded` stay settable. `SetXForwarded` runs first and a rule may override its result. The legitimate need is real: with a TLS-terminating load balancer in front of sluice every backend sees `X-Forwarded-Proto: http` and applications redirect in a loop, and `X-Forwarded-Proto: https` in the annotation is the only escape hatch while there is no trusted-proxy list. The blast radius is contained: an Ingress can only reference Services in its own namespace, so a wrong value misleads only the author's own backends. `X-Request-ID` is settable too; overriding it breaks log correlation, which is documented but not enforced.

Set only. No append (Caddy `+Name`), no delete (Caddy `-Name`), no conditional or regexp replacement in the MVP.

## 5. Errors

`Parse` returns exactly these errors. Messages carry variable names and byte offsets, never the source text.

| Case | Returned error |
|---|---|
| name not accepted by `Known` | `*UnknownVariableError{Name}`; message `unknown placeholder {name}` |
| empty name `{}` | `*UnknownVariableError{Name: ""}`; message `unknown placeholder {}` |
| header variable with empty or non-token name | `*UnknownVariableError{Name: "http.request.header.X Foo"}` |
| `{` without `}`, `}` without `{`, `{` inside a reference | `fmt.Errorf("byte %d: %w", i, ErrUnbalancedBraces)` so `errors.Is` works |
| `len(src) > MaxTemplateLength` | `fmt.Errorf("template is %d bytes, max %d", len(src), MaxTemplateLength)`; wraps `ErrTemplateTooLong` if that sentinel is approved (section 8) |

How they surface:

- Compile time. For every rule, `router.Compile` runs `ValidateHeaderName` then `Parse` and returns `&RouteError{Key, Err: fmt.Errorf("request header %q: %w", rule.Name, err)}` (or `"response header"`). `Compile` also rejects duplicate names (`EqualFold`) and more than 32 rules per direction, so it stays the sole validator for snapshots built without `Translate` (static flags, tests). Nothing is published on error.
- Controller. `Translate` runs the identical checks per Ingress before building the snapshot and skips a failing Ingress with a `Warning`, logged at warn level as `ingress skipped` with `object` and `reason`, and `IngressSkipped()`. The other Ingresses are unaffected. A `Compile` rejection afterwards is the safety net: `config rejected`, `ConfigRejected()`, last valid table kept.
- Request time. Never. `Evaluate` cannot fail and the zero `Template` renders `""`. The placeholders package has no logger and no metrics.

## 6. Security limits

- Header injection and response splitting: CR and LF are stripped from every result. Go's transport and `ResponseWriter` also refuse invalid header values (verified: `net/http: invalid header field value`), so the strip protects availability (no 502 from a crafted request) as much as integrity. Response rules run in `ModifyResponse` on `resp.Header` before the status line is written, so there is no split point.
- Cardinality: evaluated values never become metric labels. Labels are the route key and the status code only (CLAUDE.md). The observability package never sees a `Template` or a `Context`.
- Log leakage: neither template sources nor evaluated values are ever logged. Access log fields stay method, host, path, status, duration, route key and upstream. Parse errors expose a variable name and an offset, nothing else.
- Untrusted input, two levels. Templates are written by namespace-scoped cluster users: bounded by `MaxTemplateLength`, 32 rules, 256-byte names, the deny-list, and a closed registry with no `{env.*}`, `{file.*}` or `{system.*}` family, because those read the proxy's own environment and filesystem. Values come from the internet: bounded by `MaxValueLength`, sanitized, and never interpreted (no recursion, no functions, no conditionals).
- Trust boundary: `{http.request.remote}` and `{http.request.header.X-Forwarded-For}` are what the peer sent, not an authenticated client identity.

## 7. Test matrix

`Parse(src)`. "ok N" means success with N segments.

| # | src | Expected |
|---|---|---|
| 1 | `""` | ok 0, `IsStatic()` true, `Evaluate` -> `""` |
| 2 | `hello` | ok 1, static |
| 3 | `{http.request.host}` | ok 1, not static |
| 4 | `{http.request.header.User-Agent}` | ok 1 |
| 5 | `{http.request.header.user-agent}` | ok 1; evaluates identically to row 4 |
| 6 | `{http.request.header.X-Foo}` | ok 1 |
| 7 | `{http.request.header.}` | `*UnknownVariableError{Name: "http.request.header."}` |
| 8 | `{http.request.header.X Foo}` | `*UnknownVariableError` (space is not a tchar) |
| 9 | `{unknown}` | `*UnknownVariableError{Name: "unknown"}` |
| 10 | `{}` | `*UnknownVariableError{Name: ""}` |
| 11 | `{` | `ErrUnbalancedBraces` (byte 0) |
| 12 | `}` | `ErrUnbalancedBraces` (byte 0) |
| 13 | `a{{b}}c` | `ErrUnbalancedBraces` (second `{` at byte 2) |
| 14 | `{a{b}}` | `ErrUnbalancedBraces` (byte 2) |
| 15 | `x{http.request.host` | `ErrUnbalancedBraces` (opened at byte 1, never closed) |
| 16 | `{ http.request.host }` | `*UnknownVariableError` (no trimming inside braces) |
| 17 | `{HTTP.REQUEST.HOST}` | `*UnknownVariableError` (fixed names are case-sensitive) |
| 18 | `{http.request.remote}:{http.request.method} {http.request.path}` | ok 5 |
| 19 | `{k8s.namespace}/{k8s.service}` | ok 3 |
| 20 | `strings.Repeat("a", 4096)` | ok 1 (equal to the cap is allowed) |
| 21 | `strings.Repeat("a", 4097)` | too long |
| 22 | `strings.Repeat("{k8s.route}", 373)` (4103 bytes) | too long; length is checked before scanning |

`Template.Evaluate(c)`. `req` is `httptest.NewRequest` unless stated.

| # | Template and context | Expected |
|---|---|---|
| 23 | `{http.request.host}`, `Host: Example.com:8080` | `Example.com` |
| 24 | `{http.request.host}`, `Host: [::1]:8080` | `[::1]` |
| 25 | `{http.request.host}`, `Host: example.com` | `example.com` |
| 26 | `{http.request.remote}`, `RemoteAddr: 10.0.0.1:34567` | `10.0.0.1` |
| 27 | `{http.request.remote}`, `RemoteAddr: [::1]:5000` | `::1` |
| 28 | `{http.request.remote}`, `RemoteAddr: bogus` | `bogus` (raw fallback) |
| 29 | `{http.request.path}`, target `/a%20b/%2F` | `/a%20b/%2F` (not `/a b//`) |
| 30 | `{http.request.path}`, target `/a%0D%0Ab` | `/a%0D%0Ab` |
| 31 | `{http.request.header.X-Multi}`, values `a`, `b` | `a, b` |
| 32 | `{http.request.header.X-Missing}` | `""` (rule not set) |
| 33 | `{http.request.header.X-Evil}`, value `a\r\nInjected: yes` set directly on the map | `aInjected: yes` |
| 34 | `{http.request.host}`, `Context{Request: nil}` | `""` |
| 35 | `{upstream.address}`, `UpstreamAddress: ""` | `""` |
| 36 | `{k8s.route}`, header value `{k8s.service}` copied via `{http.request.header.X}` | literal `{k8s.service}` (no recursion) |
| 37 | `{http.request.header.A}` repeated 200 times, `A` = 100 bytes | `""` (20000 > 8192, dropped) |

`ValidateHeaderName(name)`.

| # | name | Expected |
|---|---|---|
| 38 | `X-Foo` | ok |
| 39 | `x-foo` | ok |
| 40 | `X Foo` | error (space) |
| 41 | `""` | error (empty) |
| 42 | `X-Foo:` | error (`:` is not a tchar) |
| 43 | `Host`, `host`, `HOST` | error (denied, case-insensitive) |
| 44 | `Content-Length`, `TE`, `Proxy-Connection` | error (denied) |
| 45 | `X-Forwarded-Proto` | ok (deliberately settable) |
| 46 | 257 bytes of `a` | error (too long) |

Annotation parsing in `Translate` (value of `sluice.dev/request-headers`).

| # | Annotation | Expected |
|---|---|---|
| 47 | `"X-A: 1\n\nX-B: {http.request.host}\n"` | 2 rules, blank line skipped |
| 48 | `"X-A: 1\nx-a: 2"` | Ingress skipped: duplicate header `X-A` |
| 49 | `"X-A"` | skipped: missing `:` on line 1 |
| 50 | `"X-A:"` | skipped: empty value on line 1 |
| 51 | `"X-A: a:b"` | 1 rule, value `a:b` |
| 52 | `"\tX-A :  v  \r\n"` | 1 rule, name `X-A`, value `v` |
| 53 | 33 lines `X-N: v` | skipped: more than 32 rules |
| 54 | `"X-A: {nope}"` | skipped; reason contains `unknown placeholder {nope}` and line 1, not the value |

## 8. Contract review

`Parse`, `Evaluate`, `IsStatic`, `Known`, `ValidateHeaderName` and `Context` are sufficient as frozen. `Known` carries the token rule for the header family, which lets `Parse` report every bad name through the one exported error type. `ValidateHeaderName` returning `error` (not `bool`) lets the deny-list and the token check give distinct messages. `Context.Route` being `"namespace/name"` matches the constant's comment. `IsStatic` is used by tests and may let `Compile` skip work; nothing depends on it.

Two additive requests, neither changing an existing signature. If they are not approved, implement both as unexported and keep the documented behaviour.

1. `var ErrTemplateTooLong = errors.New("placeholder template exceeds MaxTemplateLength")` so callers and tests use `errors.Is` for the third failure class instead of matching a message.
2. `const MaxValueLength = 8192` so the evaluated-length cap is visible next to `MaxTemplateLength` and the proxy tests can reference it.
