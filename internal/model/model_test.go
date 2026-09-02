package model

import (
	"errors"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sampleSnapshot returns a snapshot with enough variety to exercise every
// sort key: three hosts, shared paths, both path types, two backends with
// several endpoints, and header rules on one route.
func sampleSnapshot() Snapshot {
	created := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	return Snapshot{
		Routes: []Route{
			{
				Key:      RouteKey{Namespace: "shop", Name: "web", Rule: 0, Path: 1},
				Host:     "shop.example.com",
				Path:     "/api",
				PathType: PathPrefix,
				Backend:  BackendKey{Namespace: "shop", Service: "api", Port: 8080},
				RequestHeaders: []HeaderRule{
					{Name: "X-Route", Value: "{k8s.route}"},
					{Name: "X-Client-IP", Value: "{http.request.remote}"},
				},
				ResponseHeaders: []HeaderRule{
					{Name: "X-Served-By", Value: "{k8s.namespace}/{k8s.service}"},
				},
				Created: created,
			},
			{
				Key:      RouteKey{Namespace: "shop", Name: "web", Rule: 0, Path: 0},
				Host:     "shop.example.com",
				Path:     "/",
				PathType: PathPrefix,
				Backend:  BackendKey{Namespace: "shop", Service: "web", Port: 80},
				Created:  created,
			},
			{
				Key:      RouteKey{Namespace: "shop", Name: "web", Rule: 0, Path: 2},
				Host:     "shop.example.com",
				Path:     "/api",
				PathType: PathExact,
				Backend:  BackendKey{Namespace: "shop", Service: "api", Port: 8080},
				Created:  created,
			},
			{
				Key:      RouteKey{Namespace: "blog", Name: "blog", Rule: 0, Path: 0},
				Host:     "*.example.com",
				Path:     "/",
				PathType: PathPrefix,
				Backend:  BackendKey{Namespace: "blog", Service: "blog", Port: 80},
				Created:  created.Add(time.Hour),
			},
			{
				Key:      RouteKey{Namespace: "default", Name: "catch-all", Rule: 0, Path: 0},
				Host:     "",
				Path:     "/",
				PathType: PathPrefix,
				Backend:  BackendKey{Namespace: "default", Service: "fallback", Port: 80},
				Created:  created.Add(2 * time.Hour),
			},
		},
		Backends: []Backend{
			{
				Key: BackendKey{Namespace: "shop", Service: "web", Port: 80},
				Endpoints: []Endpoint{
					{Address: "10.0.0.3:80", Ready: true},
					{Address: "10.0.0.1:80", Ready: true},
					{Address: "10.0.0.2:80", Ready: false},
				},
			},
			{
				Key: BackendKey{Namespace: "shop", Service: "api", Port: 8080},
				Endpoints: []Endpoint{
					{Address: "10.0.1.2:8080", Ready: true},
					{Address: "10.0.1.1:8080", Ready: true},
				},
			},
			{
				Key:       BackendKey{Namespace: "blog", Service: "blog", Port: 80},
				Endpoints: []Endpoint{{Address: "10.0.2.1:80", Ready: true}},
			},
			{
				Key: BackendKey{Namespace: "default", Service: "fallback", Port: 80},
			},
		},
	}
}

// shuffle randomizes every slice of the snapshot in place, except header
// rules whose order is meaningful.
func shuffle(rng *rand.Rand, s *Snapshot) {
	rng.Shuffle(len(s.Routes), func(i, j int) { s.Routes[i], s.Routes[j] = s.Routes[j], s.Routes[i] })
	rng.Shuffle(len(s.Backends), func(i, j int) { s.Backends[i], s.Backends[j] = s.Backends[j], s.Backends[i] })
	for b := range s.Backends {
		eps := s.Backends[b].Endpoints
		rng.Shuffle(len(eps), func(i, j int) { eps[i], eps[j] = eps[j], eps[i] })
	}
}

func TestNormalizeSortsEveryLevel(t *testing.T) {
	s := sampleSnapshot()
	s.Normalize()

	gotRoutes := make([]string, 0, len(s.Routes))
	for _, r := range s.Routes {
		gotRoutes = append(gotRoutes, r.Host+" "+r.Path+" "+r.PathType.String()+" "+r.Key.String())
	}
	wantRoutes := []string{
		" / Prefix default/catch-all#r0p0",
		"*.example.com / Prefix blog/blog#r0p0",
		"shop.example.com / Prefix shop/web#r0p0",
		"shop.example.com /api Prefix shop/web#r0p1",
		"shop.example.com /api Exact shop/web#r0p2",
	}
	if !reflect.DeepEqual(gotRoutes, wantRoutes) {
		t.Errorf("routes order\n got %q\nwant %q", gotRoutes, wantRoutes)
	}

	gotBackends := make([]string, 0, len(s.Backends))
	for _, b := range s.Backends {
		gotBackends = append(gotBackends, b.Key.String())
	}
	wantBackends := []string{"blog/blog:80", "default/fallback:80", "shop/api:8080", "shop/web:80"}
	if !reflect.DeepEqual(gotBackends, wantBackends) {
		t.Errorf("backends order\n got %q\nwant %q", gotBackends, wantBackends)
	}

	wantEndpoints := []Endpoint{
		{Address: "10.0.0.1:80", Ready: true},
		{Address: "10.0.0.2:80", Ready: false},
		{Address: "10.0.0.3:80", Ready: true},
	}
	if got := s.Backends[3].Endpoints; !reflect.DeepEqual(got, wantEndpoints) {
		t.Errorf("endpoints order\n got %v\nwant %v", got, wantEndpoints)
	}
}

func TestNormalizeKeepsHeaderRuleOrder(t *testing.T) {
	s := sampleSnapshot()
	s.Normalize()
	var apiPrefix *Route
	for i := range s.Routes {
		if s.Routes[i].Key.Path == 1 {
			apiPrefix = &s.Routes[i]
		}
	}
	if apiPrefix == nil {
		t.Fatal("route shop/web#r0p1 not found after Normalize")
	}
	want := []HeaderRule{
		{Name: "X-Route", Value: "{k8s.route}"},
		{Name: "X-Client-IP", Value: "{http.request.remote}"},
	}
	if !reflect.DeepEqual(apiPrefix.RequestHeaders, want) {
		t.Errorf("request headers reordered: got %v want %v", apiPrefix.RequestHeaders, want)
	}
}

func TestNormalizeIdempotent(t *testing.T) {
	s := sampleSnapshot()
	s.Normalize()
	once := s.Hash()
	first := sampleSnapshot()
	first.Normalize()
	s.Normalize()
	if got := s.Hash(); got != once {
		t.Errorf("second Normalize changed the hash: %s then %s", once, got)
	}
	if !reflect.DeepEqual(s, first) {
		t.Errorf("second Normalize changed the snapshot")
	}
}

func TestNormalizeOrderIndependent(t *testing.T) {
	ref := sampleSnapshot()
	ref.Normalize()
	want := ref.Hash()

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 50; i++ {
		s := sampleSnapshot()
		shuffle(rng, &s)
		s.Normalize()
		if got := s.Hash(); got != want {
			t.Fatalf("iteration %d: shuffled snapshot hashes to %s, want %s", i, got, want)
		}
		if !reflect.DeepEqual(s, ref) {
			t.Fatalf("iteration %d: shuffled snapshot differs after Normalize", i)
		}
	}
}

func TestNormalizeNilAndEmpty(t *testing.T) {
	var nilSnap *Snapshot
	nilSnap.Normalize() // must not panic
	empty := Snapshot{}
	empty.Normalize()
	if empty.Routes != nil || empty.Backends != nil {
		t.Errorf("Normalize allocated slices on an empty snapshot: %+v", empty)
	}
}

func TestHashShape(t *testing.T) {
	s := sampleSnapshot()
	s.Normalize()
	h := s.Hash()
	if len(h) != 16 {
		t.Fatalf("hash %q has length %d, want 16 hex digits", h, len(h))
	}
	if strings.Trim(h, "0123456789abcdef") != "" {
		t.Errorf("hash %q is not lowercase hex", h)
	}
	if again := s.Hash(); again != h {
		t.Errorf("Hash is not stable: %s then %s", h, again)
	}
}

func TestHashNilEmptyAndZeroLengthAgree(t *testing.T) {
	var nilSnap *Snapshot
	empty := Snapshot{}
	zeroLen := Snapshot{Routes: []Route{}, Backends: []Backend{}}
	if a, b := nilSnap.Hash(), empty.Hash(); a != b {
		t.Errorf("nil snapshot hashes to %s, empty to %s", a, b)
	}
	if a, b := empty.Hash(), zeroLen.Hash(); a != b {
		t.Errorf("empty snapshot hashes to %s, zero-length slices to %s", a, b)
	}
	// Two routes with nil and empty header slices are the same state.
	withNil := Snapshot{Routes: []Route{{Path: "/"}}}
	withEmpty := Snapshot{Routes: []Route{{Path: "/", RequestHeaders: []HeaderRule{}, ResponseHeaders: []HeaderRule{}}}}
	if a, b := withNil.Hash(), withEmpty.Hash(); a != b {
		t.Errorf("nil header slices hash to %s, empty ones to %s", a, b)
	}
}

func TestHashDiffersWhenAnyFieldDiffers(t *testing.T) {
	base := sampleSnapshot()
	base.Normalize()
	want := base.Hash()

	tests := []struct {
		name   string
		mutate func(s *Snapshot)
	}{
		{"route host", func(s *Snapshot) { s.Routes[2].Host = "store.example.com" }},
		{"route path", func(s *Snapshot) { s.Routes[3].Path = "/api/v2" }},
		{"route path type", func(s *Snapshot) { s.Routes[3].PathType = PathExact }},
		{"route key namespace", func(s *Snapshot) { s.Routes[3].Key.Namespace = "other" }},
		{"route key name", func(s *Snapshot) { s.Routes[3].Key.Name = "web2" }},
		{"route key rule", func(s *Snapshot) { s.Routes[3].Key.Rule = 7 }},
		{"route key path", func(s *Snapshot) { s.Routes[3].Key.Path = 7 }},
		{"route backend service", func(s *Snapshot) { s.Routes[3].Backend.Service = "api2" }},
		{"route backend port", func(s *Snapshot) { s.Routes[3].Backend.Port = 9090 }},
		{"route created", func(s *Snapshot) { s.Routes[3].Created = s.Routes[3].Created.Add(time.Second) }},
		{"request header name", func(s *Snapshot) { s.Routes[3].RequestHeaders[0].Name = "X-Route-2" }},
		{"request header value", func(s *Snapshot) { s.Routes[3].RequestHeaders[1].Value = "static" }},
		{"request header order", func(s *Snapshot) {
			h := s.Routes[3].RequestHeaders
			h[0], h[1] = h[1], h[0]
		}},
		{"request header removed", func(s *Snapshot) { s.Routes[3].RequestHeaders = s.Routes[3].RequestHeaders[:1] }},
		{"response header value", func(s *Snapshot) { s.Routes[3].ResponseHeaders[0].Value = "{k8s.service}" }},
		{"response header moved to request", func(s *Snapshot) {
			s.Routes[3].RequestHeaders = append(s.Routes[3].RequestHeaders, s.Routes[3].ResponseHeaders...)
			s.Routes[3].ResponseHeaders = nil
		}},
		{"route removed", func(s *Snapshot) { s.Routes = s.Routes[1:] }},
		{"route added", func(s *Snapshot) { s.Routes = append(s.Routes, Route{Path: "/extra"}) }},
		{"backend port", func(s *Snapshot) { s.Backends[3].Key.Port = 8080 }},
		{"backend service", func(s *Snapshot) { s.Backends[3].Key.Service = "web2" }},
		{"backend namespace", func(s *Snapshot) { s.Backends[3].Key.Namespace = "shop2" }},
		{"endpoint address", func(s *Snapshot) { s.Backends[3].Endpoints[0].Address = "10.0.0.9:80" }},
		{"endpoint port in address", func(s *Snapshot) { s.Backends[3].Endpoints[0].Address = "10.0.0.1:81" }},
		{"endpoint ready flag", func(s *Snapshot) { s.Backends[3].Endpoints[1].Ready = true }},
		{"endpoint removed", func(s *Snapshot) { s.Backends[3].Endpoints = s.Backends[3].Endpoints[:2] }},
		{"endpoint added", func(s *Snapshot) {
			s.Backends[1].Endpoints = append(s.Backends[1].Endpoints, Endpoint{Address: "10.0.9.1:80", Ready: true})
		}},
		{"backend removed", func(s *Snapshot) { s.Backends = s.Backends[:3] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sampleSnapshot()
			s.Normalize()
			tt.mutate(&s)
			s.Normalize()
			if got := s.Hash(); got == want {
				t.Errorf("hash unchanged (%s) after mutating %s", got, tt.name)
			}
		})
	}
}

// TestHashFieldBoundaries checks that the serialization is prefix-free: moving
// bytes between adjacent fields, or elements between adjacent slices, must
// change the hash even though the concatenated bytes are identical.
func TestHashFieldBoundaries(t *testing.T) {
	pairs := []struct {
		name string
		a, b Snapshot
	}{
		{
			name: "host and path share bytes",
			a:    Snapshot{Routes: []Route{{Host: "ab", Path: "c"}}},
			b:    Snapshot{Routes: []Route{{Host: "a", Path: "bc"}}},
		},
		{
			name: "header name and value share bytes",
			a:    Snapshot{Routes: []Route{{RequestHeaders: []HeaderRule{{Name: "X-", Value: "Av"}}}}},
			b:    Snapshot{Routes: []Route{{RequestHeaders: []HeaderRule{{Name: "X-A", Value: "v"}}}}},
		},
		{
			name: "request versus response header",
			a:    Snapshot{Routes: []Route{{RequestHeaders: []HeaderRule{{Name: "X-A", Value: "v"}}}}},
			b:    Snapshot{Routes: []Route{{ResponseHeaders: []HeaderRule{{Name: "X-A", Value: "v"}}}}},
		},
		{
			name: "one route with empty fields versus none",
			a:    Snapshot{Routes: []Route{{}}},
			b:    Snapshot{},
		},
		{
			name: "one backend versus none",
			a:    Snapshot{Backends: []Backend{{}}},
			b:    Snapshot{},
		},
		{
			name: "endpoint split across backends",
			a: Snapshot{Backends: []Backend{
				{Key: BackendKey{Service: "a"}, Endpoints: []Endpoint{{Address: "x"}, {Address: "y"}}},
				{Key: BackendKey{Service: "b"}},
			}},
			b: Snapshot{Backends: []Backend{
				{Key: BackendKey{Service: "a"}, Endpoints: []Endpoint{{Address: "x"}}},
				{Key: BackendKey{Service: "b"}, Endpoints: []Endpoint{{Address: "y"}}},
			}},
		},
		{
			name: "route key namespace and name share bytes",
			a:    Snapshot{Routes: []Route{{Key: RouteKey{Namespace: "ab", Name: "c"}}}},
			b:    Snapshot{Routes: []Route{{Key: RouteKey{Namespace: "a", Name: "bc"}}}},
		},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			p.a.Normalize()
			p.b.Normalize()
			if ha, hb := p.a.Hash(), p.b.Hash(); ha == hb {
				t.Errorf("distinct snapshots share hash %s", ha)
			}
		})
	}
}

func TestParseHeaderRulesValid(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []HeaderRule
	}{
		{name: "empty annotation", text: "", want: nil},
		{name: "only whitespace and newlines", text: "\n  \n\t\n", want: nil},
		{
			// placeholders.md row 47
			name: "blank line skipped",
			text: "X-A: 1\n\nX-B: {http.request.host}\n",
			want: []HeaderRule{{Name: "X-A", Value: "1"}, {Name: "X-B", Value: "{http.request.host}"}},
		},
		{
			// placeholders.md row 51
			name: "value may contain colons",
			text: "X-A: a:b",
			want: []HeaderRule{{Name: "X-A", Value: "a:b"}},
		},
		{
			// placeholders.md row 52
			name: "surrounding whitespace and CRLF trimmed",
			text: "\tX-A :  v  \r\n",
			want: []HeaderRule{{Name: "X-A", Value: "v"}},
		},
		{
			name: "order preserved",
			text: "X-C: 3\nX-A: 1\nX-B: 2",
			want: []HeaderRule{{Name: "X-C", Value: "3"}, {Name: "X-A", Value: "1"}, {Name: "X-B", Value: "2"}},
		},
		{
			name: "no space after colon",
			text: "X-A:v",
			want: []HeaderRule{{Name: "X-A", Value: "v"}},
		},
		{
			// placeholders.md row 54 at the model level: templates are the
			// router's job, so an unknown placeholder parses here.
			name: "template not checked here",
			text: "X-A: {nope}",
			want: []HeaderRule{{Name: "X-A", Value: "{nope}"}},
		},
		{
			// Token validation is the placeholders package's job; the model
			// only checks structure.
			name: "name not checked as token here",
			text: "# note: x",
			want: []HeaderRule{{Name: "# note", Value: "x"}},
		},
		{
			name: "exactly MaxHeaderRules rules",
			text: repeatRules(MaxHeaderRules),
			want: expectRules(MaxHeaderRules),
		},
		{
			name: "name of exactly MaxHeaderNameLength bytes",
			text: strings.Repeat("a", MaxHeaderNameLength) + ": v",
			want: []HeaderRule{{Name: strings.Repeat("a", MaxHeaderNameLength), Value: "v"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHeaderRules(tt.text)
			if err != nil {
				t.Fatalf("ParseHeaderRules returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rules\n got %v\nwant %v", got, tt.want)
			}
			if err := ValidateHeaderRules(got); err != nil {
				t.Errorf("parsed rules fail ValidateHeaderRules: %v", err)
			}
		})
	}
}

func TestParseHeaderRulesInvalid(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		wantContain []string // fragments the error must carry
		wantAbsent  []string // fragments the error must never carry (values)
	}{
		{
			// placeholders.md row 48
			name:        "duplicate name case-insensitive",
			text:        "X-A: secret1\nx-a: secret2",
			wantContain: []string{"line 2", "x-a", "duplicate", "line 1"},
			wantAbsent:  []string{"secret1", "secret2"},
		},
		{
			// placeholders.md row 49
			name:        "missing colon",
			text:        "X-A",
			wantContain: []string{"line 1", ":"},
		},
		{
			name:        "missing colon on a later line",
			text:        "X-A: 1\nX-B\n",
			wantContain: []string{"line 2"},
			wantAbsent:  []string{"X-B"},
		},
		{
			// placeholders.md row 50
			name:        "empty value",
			text:        "X-A:",
			wantContain: []string{"line 1", "X-A", "empty value"},
		},
		{
			name:        "whitespace-only value",
			text:        "X-A:   \t ",
			wantContain: []string{"line 1", "empty value"},
		},
		{
			name:        "empty name",
			text:        ": secretvalue",
			wantContain: []string{"line 1", "empty header name"},
			wantAbsent:  []string{"secretvalue"},
		},
		{
			name:        "empty name after blank lines",
			text:        "\n\n : v",
			wantContain: []string{"line 3", "empty header name"},
		},
		{
			// placeholders.md row 53
			name:        "more than MaxHeaderRules",
			text:        repeatRules(MaxHeaderRules + 1),
			wantContain: []string{"line 33", "more than 32"},
		},
		{
			name:        "name longer than MaxHeaderNameLength",
			text:        strings.Repeat("a", MaxHeaderNameLength+1) + ": topsecret",
			wantContain: []string{"line 1", "257 bytes", "max 256"},
			wantAbsent:  []string{"topsecret", strings.Repeat("a", MaxHeaderNameLength+1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHeaderRules(tt.text)
			if err == nil {
				t.Fatalf("ParseHeaderRules returned %v, want error", got)
			}
			if got != nil {
				t.Errorf("ParseHeaderRules returned rules %v alongside the error", got)
			}
			msg := err.Error()
			for _, frag := range tt.wantContain {
				if !strings.Contains(msg, frag) {
					t.Errorf("error %q does not mention %q", msg, frag)
				}
			}
			for _, frag := range tt.wantAbsent {
				if strings.Contains(msg, frag) {
					t.Errorf("error %q echoes %q", msg, frag)
				}
			}
		})
	}
}

func TestValidateHeaderRules(t *testing.T) {
	tests := []struct {
		name        string
		rules       []HeaderRule
		wantErr     bool
		wantContain []string
		wantAbsent  []string
	}{
		{name: "nil", rules: nil},
		{name: "empty", rules: []HeaderRule{}},
		{name: "one rule", rules: []HeaderRule{{Name: "X-A", Value: "v"}}},
		{
			name:  "distinct names differing beyond case",
			rules: []HeaderRule{{Name: "X-A", Value: "1"}, {Name: "X-B", Value: "2"}},
		},
		{name: "exactly MaxHeaderRules", rules: expectRules(MaxHeaderRules)},
		{name: "name at MaxHeaderNameLength", rules: []HeaderRule{{Name: strings.Repeat("a", MaxHeaderNameLength), Value: "v"}}},
		{
			name:        "empty name",
			rules:       []HeaderRule{{Name: "X-A", Value: "1"}, {Name: "", Value: "hidden"}},
			wantErr:     true,
			wantContain: []string{"rule 1", "empty header name"},
			wantAbsent:  []string{"hidden"},
		},
		{
			name:        "empty value",
			rules:       []HeaderRule{{Name: "X-A", Value: ""}},
			wantErr:     true,
			wantContain: []string{"rule 0", "X-A", "empty value"},
		},
		{
			name:        "duplicate case-insensitive",
			rules:       []HeaderRule{{Name: "X-A", Value: "s1"}, {Name: "X-B", Value: "s2"}, {Name: "x-A", Value: "s3"}},
			wantErr:     true,
			wantContain: []string{"rule 2", "x-A", "duplicate", "rule 0"},
			wantAbsent:  []string{"s1", "s2", "s3"},
		},
		{
			name:        "too many rules",
			rules:       expectRules(MaxHeaderRules + 1),
			wantErr:     true,
			wantContain: []string{"33 header rules", "max 32"},
		},
		{
			name:        "name too long",
			rules:       []HeaderRule{{Name: strings.Repeat("b", MaxHeaderNameLength+1), Value: "v"}},
			wantErr:     true,
			wantContain: []string{"rule 0", "257 bytes"},
			wantAbsent:  []string{strings.Repeat("b", MaxHeaderNameLength+1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateHeaderRules(tt.rules)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateHeaderRules returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateHeaderRules returned nil, want error")
			}
			msg := err.Error()
			for _, frag := range tt.wantContain {
				if !strings.Contains(msg, frag) {
					t.Errorf("error %q does not mention %q", msg, frag)
				}
			}
			for _, frag := range tt.wantAbsent {
				if strings.Contains(msg, frag) {
					t.Errorf("error %q echoes %q", msg, frag)
				}
			}
		})
	}
}

func TestValidateHeaderRulesErrorsWrapContext(t *testing.T) {
	err := ValidateHeaderRules([]HeaderRule{{Name: "", Value: "v"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	// The per-rule error is wrapped, so unwrapping reaches the inner cause.
	if errors.Unwrap(err) == nil {
		t.Errorf("error %q is not wrapped", err)
	}
}

// repeatRules builds n lines "X-<i>: v".
func repeatRules(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("X-")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(": v\n")
	}
	return b.String()
}

// expectRules is the parsed form of repeatRules(n).
func expectRules(n int) []HeaderRule {
	rules := make([]HeaderRule, 0, n)
	for i := 0; i < n; i++ {
		rules = append(rules, HeaderRule{Name: "X-" + strconv.Itoa(i), Value: "v"})
	}
	return rules
}
