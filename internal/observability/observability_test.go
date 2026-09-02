package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Label cardinality: every assertion below uses a route key, a status code
// or an error kind as label. If a test ever needs a host, a path, a header
// value or an upstream address as a label, the design has been broken.

var hexID32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// captureHandler records what the wrapped handler saw on the request.
type captureHandler struct {
	headerID  string
	headerAll []string
	ctxID     string
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.headerID = r.Header.Get(HeaderRequestID)
	c.headerAll = r.Header.Values(HeaderRequestID)
	c.ctxID = RequestIDFrom(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func TestRequestIDGeneratesAndEchoes(t *testing.T) {
	inner := &captureHandler{}
	h := RequestID(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := rec.Header().Get(HeaderRequestID)
	if !hexID32.MatchString(got) {
		t.Fatalf("response %s = %q, want 32 hex characters", HeaderRequestID, got)
	}
	if inner.headerID != got {
		t.Errorf("request header id = %q, want %q", inner.headerID, got)
	}
	if inner.ctxID != got {
		t.Errorf("context id = %q, want %q", inner.ctxID, got)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	// Two requests never share an id.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Header().Get(HeaderRequestID) == got {
		t.Errorf("two generated ids are equal: %q", got)
	}
}

func TestRequestIDKeepsValidClientID(t *testing.T) {
	valid := []string{
		"abc",
		"req-123",
		"a.b_c-D9",
		"0123456789abcdefABCDEF",
		strings.Repeat("x", 128),
	}
	for _, id := range valid {
		t.Run(id[:min(len(id), 24)], func(t *testing.T) {
			inner := &captureHandler{}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(HeaderRequestID, id)
			rec := httptest.NewRecorder()
			RequestID(inner).ServeHTTP(rec, req)

			if got := rec.Header().Get(HeaderRequestID); got != id {
				t.Errorf("response id = %q, want client id kept", got)
			}
			if inner.headerID != id || inner.ctxID != id {
				t.Errorf("inner saw header %q ctx %q, want %q", inner.headerID, inner.ctxID, id)
			}
		})
	}
}

func TestRequestIDReplacesInvalidClientID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"too long", strings.Repeat("x", 129)},
		{"space", "abc def"},
		{"slash", "a/b"},
		{"colon", "a:b"},
		{"non ascii", "café"},
		{"control", "abc\x01"},
		{"cr lf", "abc\r\ndef"},
		{"braces", "{abc}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &captureHandler{}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header[HeaderRequestID] = []string{tc.id}
			rec := httptest.NewRecorder()
			RequestID(inner).ServeHTTP(rec, req)

			got := rec.Header().Get(HeaderRequestID)
			if got == tc.id {
				t.Fatalf("invalid client id %q was kept", tc.id)
			}
			if !hexID32.MatchString(got) {
				t.Fatalf("replacement id = %q, want 32 hex characters", got)
			}
			if inner.headerID != got || inner.ctxID != got {
				t.Errorf("inner saw header %q ctx %q, want %q", inner.headerID, inner.ctxID, got)
			}
		})
	}
}

func TestRequestIDCollapsesRepeatedHeader(t *testing.T) {
	inner := &captureHandler{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Add(HeaderRequestID, "first")
	req.Header.Add(HeaderRequestID, "second")
	rec := httptest.NewRecorder()
	RequestID(inner).ServeHTTP(rec, req)

	if len(inner.headerAll) != 1 || inner.headerAll[0] != "first" {
		t.Errorf("forwarded header values = %q, want [first]", inner.headerAll)
	}
	if got := rec.Header().Get(HeaderRequestID); got != "first" {
		t.Errorf("response id = %q, want first", got)
	}
}

func TestRequestIDFromMissing(t *testing.T) {
	if got := RequestIDFrom(t.Context()); got != "" {
		t.Errorf("RequestIDFrom(empty ctx) = %q, want \"\"", got)
	}
}

func TestNewLoggerJSONWithLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, slog.LevelInfo, true)

	log.Debug("hidden", "k", "v")
	log.Info("shown", "route", "ns/name#r0p1", "status", 200)
	log.Warn("also shown")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 is not JSON: %v\n%s", err, lines[0])
	}
	if first["level"] != "INFO" || first["msg"] != "shown" {
		t.Errorf("line 0 = %v, want level INFO msg shown", first)
	}
	if first["route"] != "ns/name#r0p1" || first["status"] != float64(200) {
		t.Errorf("line 0 attributes = %v", first)
	}
	if _, ok := first["source"]; ok {
		t.Errorf("line 0 carries a source attribute, AddSource must be off")
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 1 is not JSON: %v\n%s", err, lines[1])
	}
	if second["level"] != "WARN" {
		t.Errorf("line 1 level = %v, want WARN", second["level"])
	}
	if strings.Contains(buf.String(), "hidden") {
		t.Errorf("debug line leaked through the info filter")
	}
}

func TestNewLoggerText(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, slog.LevelDebug, false)
	log.Debug("visible", "k", "v")

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "msg=visible") || !strings.Contains(out, "k=v") {
		t.Errorf("text output = %q", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text handler produced JSON: %q", out)
	}
}

func TestNewLoggerHonoursLevel(t *testing.T) {
	log := NewLogger(slog.LevelWarn, true)
	ctx := t.Context()
	if log.Enabled(ctx, slog.LevelInfo) {
		t.Errorf("info enabled at warn level")
	}
	if !log.Enabled(ctx, slog.LevelWarn) {
		t.Errorf("warn disabled at warn level")
	}
}

// gather returns every metric family from g, failing the test on error.
func gather(t *testing.T, g prometheus.Gatherer) []*dto.MetricFamily {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return fams
}

// family returns the named family or nil.
func family(fams []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range fams {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// metricWithLabels returns the metric in fam whose label pairs equal want, or
// nil. want must list every label of the metric.
func metricWithLabels(fam *dto.MetricFamily, want map[string]string) *dto.Metric {
	if fam == nil {
		return nil
	}
next:
	for _, m := range fam.GetMetric() {
		if len(m.GetLabel()) != len(want) {
			continue
		}
		for _, lp := range m.GetLabel() {
			if want[lp.GetName()] != lp.GetValue() {
				continue next
			}
		}
		return m
	}
	return nil
}

func counterValue(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	m := metricWithLabels(family(gather(t, g), name), labels)
	if m == nil {
		t.Fatalf("metric %s%v not found", name, labels)
	}
	return m.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	m := metricWithLabels(family(gather(t, g), name), map[string]string{})
	if m == nil {
		t.Fatalf("gauge %s not found", name)
	}
	return m.GetGauge().GetValue()
}

func TestMetricsObserveRequest(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest("ns/app#r0p0", 200, 30*time.Millisecond)
	m.ObserveRequest("ns/app#r0p0", 200, 40*time.Millisecond)
	m.ObserveRequest("ns/app#r0p0", 502, 3*time.Second)
	m.ObserveRequest("", 404, 100*time.Microsecond)
	m.ObserveRequest("ns/app#r0p0", 499, 10*time.Millisecond)

	g := m.Gatherer()
	cases := []struct {
		route, code string
		want        float64
	}{
		{"ns/app#r0p0", "200", 2},
		{"ns/app#r0p0", "502", 1},
		{"ns/app#r0p0", "499", 1},
		{"none", "404", 1},
	}
	for _, tc := range cases {
		got := counterValue(t, g, "sluice_http_requests_total", map[string]string{"route": tc.route, "code": tc.code})
		if got != tc.want {
			t.Errorf("sluice_http_requests_total{route=%q,code=%q} = %v, want %v", tc.route, tc.code, got, tc.want)
		}
	}

	fam := family(gather(t, g), "sluice_http_request_duration_seconds")
	if fam == nil || fam.GetType() != dto.MetricType_HISTOGRAM {
		t.Fatalf("duration family = %v, want a histogram", fam)
	}
	h := metricWithLabels(fam, map[string]string{"route": "ns/app#r0p0"})
	if h == nil {
		t.Fatalf("duration{route=ns/app#r0p0} not found")
	}
	hist := h.GetHistogram()
	if hist.GetSampleCount() != 4 {
		t.Errorf("sample count = %d, want 4", hist.GetSampleCount())
	}
	wantSum := 0.03 + 0.04 + 3 + 0.01
	if diff := hist.GetSampleSum() - wantSum; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("sample sum = %v, want %v", hist.GetSampleSum(), wantSum)
	}
	// Bucket bounds: first is 1 ms, last is 10 s, cumulative counts follow
	// the observations above.
	buckets := hist.GetBucket()
	if len(buckets) == 0 || buckets[0].GetUpperBound() != 0.001 || buckets[len(buckets)-1].GetUpperBound() != 10 {
		t.Fatalf("bucket bounds = %v", bounds(buckets))
	}
	wantCumulative := map[float64]uint64{0.01: 1, 0.05: 3, 1: 3, 5: 4, 10: 4}
	for _, b := range buckets {
		if want, ok := wantCumulative[b.GetUpperBound()]; ok && b.GetCumulativeCount() != want {
			t.Errorf("bucket le=%v count = %d, want %d", b.GetUpperBound(), b.GetCumulativeCount(), want)
		}
	}
	if none := metricWithLabels(fam, map[string]string{"route": "none"}); none == nil || none.GetHistogram().GetSampleCount() != 1 {
		t.Errorf("duration{route=none} = %v, want one sample", none)
	}
}

func bounds(bs []*dto.Bucket) []float64 {
	out := make([]float64, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.GetUpperBound())
	}
	return out
}

func TestMetricsUpstreamError(t *testing.T) {
	m := NewMetrics()
	m.UpstreamError("ns/app#r0p0", "dial")
	m.UpstreamError("ns/app#r0p0", "dial")
	m.UpstreamError("ns/app#r0p0", "timeout")
	m.UpstreamError("", "")

	g := m.Gatherer()
	cases := []struct {
		route, kind string
		want        float64
	}{
		{"ns/app#r0p0", "dial", 2},
		{"ns/app#r0p0", "timeout", 1},
		{"none", "other", 1},
	}
	for _, tc := range cases {
		got := counterValue(t, g, "sluice_upstream_errors_total", map[string]string{"route": tc.route, "kind": tc.kind})
		if got != tc.want {
			t.Errorf("sluice_upstream_errors_total{route=%q,kind=%q} = %v, want %v", tc.route, tc.kind, got, tc.want)
		}
	}
}

func TestMetricsConfig(t *testing.T) {
	m := NewMetrics()
	g := m.Gatherer()

	if got := gaugeValue(t, g, "sluice_config_version"); got != 0 {
		t.Errorf("initial sluice_config_version = %v, want 0", got)
	}

	m.ConfigApplied(3, 7)
	if got := gaugeValue(t, g, "sluice_config_version"); got != 3 {
		t.Errorf("sluice_config_version = %v, want 3", got)
	}
	if got := gaugeValue(t, g, "sluice_routes"); got != 7 {
		t.Errorf("sluice_routes = %v, want 7", got)
	}
	if got := counterValue(t, g, "sluice_config_applied_total", nil); got != 1 {
		t.Errorf("sluice_config_applied_total = %v, want 1", got)
	}

	m.ConfigApplied(4, 2)
	if got := gaugeValue(t, g, "sluice_config_version"); got != 4 {
		t.Errorf("sluice_config_version = %v, want 4", got)
	}
	if got := gaugeValue(t, g, "sluice_routes"); got != 2 {
		t.Errorf("sluice_routes = %v, want 2 (gauge, not counter)", got)
	}
	if got := counterValue(t, g, "sluice_config_applied_total", nil); got != 2 {
		t.Errorf("sluice_config_applied_total = %v, want 2", got)
	}

	m.ConfigRejected()
	m.ConfigRejected()
	if got := counterValue(t, g, "sluice_config_rejected_total", nil); got != 2 {
		t.Errorf("sluice_config_rejected_total = %v, want 2", got)
	}
	m.IngressSkipped()
	if got := counterValue(t, g, "sluice_ingresses_skipped_total", nil); got != 1 {
		t.Errorf("sluice_ingresses_skipped_total = %v, want 1", got)
	}
}

func TestMetricsSetReady(t *testing.T) {
	m := NewMetrics()
	g := m.Gatherer()
	if got := gaugeValue(t, g, "sluice_ready"); got != 0 {
		t.Errorf("initial sluice_ready = %v, want 0", got)
	}
	m.SetReady(true)
	if got := gaugeValue(t, g, "sluice_ready"); got != 1 {
		t.Errorf("sluice_ready after Set(true) = %v, want 1", got)
	}
	m.SetReady(false)
	if got := gaugeValue(t, g, "sluice_ready"); got != 0 {
		t.Errorf("sluice_ready after Set(false) = %v, want 0", got)
	}
}

func TestMetricsRegistersRuntimeCollectors(t *testing.T) {
	fams := gather(t, NewMetrics().Gatherer())
	if family(fams, "go_goroutines") == nil {
		t.Errorf("go collector not registered: go_goroutines missing")
	}
	hasProcess := false
	for _, f := range fams {
		if strings.HasPrefix(f.GetName(), "process_") {
			hasProcess = true
			break
		}
	}
	if !hasProcess {
		t.Errorf("process collector not registered: no process_* family")
	}
}

// TestMetricsLabelNames pins the label sets so a change in cardinality is a
// deliberate edit of this test, not an accident.
func TestMetricsLabelNames(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest("r", 200, time.Millisecond)
	m.UpstreamError("r", "dial")
	fams := gather(t, m.Gatherer())

	want := map[string][]string{
		"sluice_http_requests_total":           {"code", "route"},
		"sluice_http_request_duration_seconds": {"route"},
		"sluice_upstream_errors_total":         {"kind", "route"},
		"sluice_config_version":                nil,
		"sluice_config_applied_total":          nil,
		"sluice_config_rejected_total":         nil,
		"sluice_ingresses_skipped_total":       nil,
		"sluice_routes":                        nil,
		"sluice_ready":                         nil,
	}
	for name, labels := range want {
		fam := family(fams, name)
		if fam == nil {
			t.Errorf("family %s missing", name)
			continue
		}
		if len(fam.GetMetric()) == 0 {
			t.Errorf("family %s has no metric", name)
			continue
		}
		var got []string
		for _, lp := range fam.GetMetric()[0].GetLabel() {
			got = append(got, lp.GetName())
		}
		if strings.Join(got, ",") != strings.Join(labels, ",") {
			t.Errorf("%s labels = %v, want %v", name, got, labels)
		}
	}
}

func TestAdminMuxHealthz(t *testing.T) {
	mux := AdminMux(&Readiness{}, NewMetrics())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("/healthz body = %q, want ok", body)
	}
}

// Scenario 14: the process is not ready until told so. The controller flips
// the flag after its informers synced; here we check the HTTP side of that.
func TestScenario14ReadyzFollowsReadinessFlag(t *testing.T) {
	ready := &Readiness{}
	metrics := NewMetrics()
	mux := AdminMux(ready, metrics)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before Set(true) = %d, want 503", rec.Code)
	}
	if ready.Ready() {
		t.Errorf("Ready() = true before Set(true)")
	}

	ready.Set(true)
	metrics.SetReady(true)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz after Set(true) = %d, want 200", rec.Code)
	}
	if got := gaugeValue(t, metrics.Gatherer(), "sluice_ready"); got != 1 {
		t.Errorf("sluice_ready = %v, want 1", got)
	}

	// Shutdown flips it back first thing.
	ready.Set(false)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after Set(false) = %d, want 503", rec.Code)
	}
}

func TestAdminMuxMetrics(t *testing.T) {
	ready := &Readiness{}
	metrics := NewMetrics()
	metrics.SetReady(true)
	metrics.ObserveRequest("ns/app#r0p0", 200, 5*time.Millisecond)
	mux := AdminMux(ready, metrics)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("/metrics Content-Type = %q, want Prometheus text exposition", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"sluice_ready 1",
		`sluice_http_requests_total{code="200",route="ns/app#r0p0"} 1`,
		"sluice_http_request_duration_seconds_bucket",
		"sluice_config_version",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body lacks %q", want)
		}
	}
}

func TestAdminMuxUnknownPath(t *testing.T) {
	mux := AdminMux(&Readiness{}, NewMetrics())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown admin path status = %d, want 404", rec.Code)
	}
}
