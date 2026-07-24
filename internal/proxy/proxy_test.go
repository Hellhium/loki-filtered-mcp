package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
)

// fakeLoki records what actually reached the upstream.
type fakeLoki struct {
	mu     sync.Mutex
	calls  int
	method string
	path   string
	form   url.Values
	header http.Header
}

func (f *fakeLoki) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm() // merges the URL query and a urlencoded body, like Loki
		f.mu.Lock()
		f.calls++
		f.method = r.Method
		f.path = r.URL.Path
		f.form = r.Form
		f.header = r.Header.Clone()
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}
}

func (f *fakeLoki) snapshot() (calls int, method, path string, form url.Values, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.method, f.path, f.form, f.header
}

type harness struct {
	proxy *httptest.Server
	loki  *fakeLoki
}

func newHarness(t *testing.T, onConflict string, enforceLabelAPIs bool, mutate ...func(*config.ResolvedInstance)) *harness {
	t.Helper()

	loki := &fakeLoki{}
	lokiSrv := httptest.NewServer(loki.handler())
	t.Cleanup(lokiSrv.Close)

	inst := config.ResolvedInstance{
		Name:  "test",
		Proxy: true,
		Filters: []config.Filter{
			{Label: "namespace", Values: []string{"team-a", "team-b"}},
		},
		Enforcement: config.Enforcement{OnConflict: onConflict, EnforceLabelAPIs: enforceLabelAPIs},
		Loki:        config.Loki{URL: lokiSrv.URL, Timeout: config.Duration(0)},
		Defaults:    config.Defaults{Limit: 100},
	}
	for _, m := range mutate {
		m(&inst)
	}

	filters, err := inst.EnforcedFilters()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(inst, enforcer.New(filters, inst.Mode()))
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	return &harness{proxy: proxySrv, loki: loki}
}

func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	return h.do(t, mustRequest(t, http.MethodGet, h.proxy.URL+path, "", ""))
}

func (h *harness) postForm(t *testing.T, path, body string) *http.Response {
	t.Helper()
	return h.do(t, mustRequest(t, http.MethodPost, h.proxy.URL+path, body, formContentType))
}

func (h *harness) do(t *testing.T, r *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func mustRequest(t *testing.T, method, rawURL, body, contentType string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const enforcedSelector = `{namespace=~"team-a|team-b"}`

// --- query enforcement ------------------------------------------------------

func TestQueryRangeEnforcedGET(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, `/loki/api/v1/query_range?query=`+url.QueryEscape(`{app="foo"}`)+`&limit=10`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, path, form, _ := h.loki.snapshot()
	if path != "/loki/api/v1/query_range" {
		t.Errorf("upstream path = %q", path)
	}
	if got := form.Get("query"); got != `{app="foo", namespace=~"team-a|team-b"}` {
		t.Errorf("query reaching Loki = %q", got)
	}
	if got := form.Get("limit"); got != "10" {
		t.Errorf("unrelated params must survive: limit = %q", got)
	}
}

// TestQueryRangeEnforcedPOSTBody is the bypass this proxy exists to prevent:
// Grafana sends query_range as an urlencoded POST, so a proxy that only
// rewrote the URL query string would let the body through unenforced.
func TestQueryRangeEnforcedPOSTBody(t *testing.T) {
	h := newHarness(t, "override", true)
	body := url.Values{"query": {`{namespace="prod-secret"}`}, "limit": {"5"}}.Encode()
	resp := h.postForm(t, "/loki/api/v1/query_range", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, method, _, form, _ := h.loki.snapshot()
	if method != http.MethodPost {
		t.Errorf("upstream method = %q", method)
	}
	if got := form.Get("query"); got != enforcedSelector {
		t.Errorf("POST body query reaching Loki = %q, want the enforced selector", got)
	}
	if got := form.Get("limit"); got != "5" {
		t.Errorf("limit = %q", got)
	}
}

// The same widening attempt must be refused identically over GET and POST.
func TestWidenedQueryRejectedBothMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			h := newHarness(t, "reject", true)
			var resp *http.Response
			if method == http.MethodGet {
				resp = h.get(t, "/loki/api/v1/query_range?query="+url.QueryEscape(`{namespace="prod-secret"}`))
			} else {
				resp = h.postForm(t, "/loki/api/v1/query_range",
					url.Values{"query": {`{namespace="prod-secret"}`}}.Encode())
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyString(t, resp))
			}
			if !strings.Contains(bodyString(t, resp), "rejected") {
				t.Errorf("unexpected error body")
			}
			if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
				t.Fatalf("Loki must not be called on a rejected query, got %d calls", calls)
			}
		})
	}
}

func TestNarrowingWithinScopeAllowed(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/query_range?query="+url.QueryEscape(`{namespace="team-a"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, _, form, _ := h.loki.snapshot()
	if got := form.Get("query"); got != `{namespace="team-a"}` {
		t.Errorf("query = %q", got)
	}
}

func TestAggregationEnforced(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/query?query="+url.QueryEscape(`sum(rate({app="api"}[5m]))`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, path, form, _ := h.loki.snapshot()
	if path != "/loki/api/v1/query" {
		t.Errorf("path = %q", path)
	}
	if got := form.Get("query"); got != `sum(rate({app="api", namespace=~"team-a|team-b"}[5m]))` {
		t.Errorf("query = %q", got)
	}
}

func TestVolumeRangeEnforced(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/index/volume_range?query="+url.QueryEscape(`{app="api"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, path, form, _ := h.loki.snapshot()
	if path != "/loki/api/v1/index/volume_range" {
		t.Errorf("path = %q", path)
	}
	if got := form.Get("query"); got != `{app="api", namespace=~"team-a|team-b"}` {
		t.Errorf("query = %q", got)
	}
}

func TestMissingQueryRejected(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/query_range?limit=10")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatalf("got %d upstream calls", calls)
	}
}

func TestInvalidLogQLRejected(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/query_range?query="+url.QueryEscape(`{app=}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(bodyString(t, resp), "invalid LogQL") {
		t.Error("expected an invalid-LogQL message")
	}
}

// --- parameter ambiguity ----------------------------------------------------

func TestParamInURLAndBodyRejected(t *testing.T) {
	h := newHarness(t, "reject", true)
	r := mustRequest(t, http.MethodPost,
		h.proxy.URL+"/loki/api/v1/query_range?query="+url.QueryEscape(`{app="a"}`),
		url.Values{"query": {`{namespace="prod-secret"}`}}.Encode(), formContentType)
	resp := h.do(t, r)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyString(t, resp))
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatalf("got %d upstream calls", calls)
	}
}

func TestDuplicateQueryParamRejected(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/query_range?query="+url.QueryEscape(`{app="a"}`)+
		"&query="+url.QueryEscape(`{namespace="prod-secret"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, bodyString(t, resp))
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatalf("got %d upstream calls", calls)
	}
}

// Grafana sends "application/x-www-form-urlencoded; charset=UTF-8"; the media
// type parameters must not defeat the content-type match.
func TestFormContentTypeWithCharset(t *testing.T) {
	h := newHarness(t, "override", true)
	r := mustRequest(t, http.MethodPost, h.proxy.URL+"/loki/api/v1/query_range",
		url.Values{"query": {`{namespace="prod-secret"}`}}.Encode(),
		"application/x-www-form-urlencoded; charset=UTF-8")
	if resp := h.do(t, r); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, _, form, _ := h.loki.snapshot()
	if got := form.Get("query"); got != enforcedSelector {
		t.Errorf("query = %q, want the enforced selector", got)
	}
}

// Whatever the caller sends, the enforced parameters must arrive in exactly one
// place: no un-enforced copy may survive on the other side of the request.
func TestPOSTLeavesNothingInTheURL(t *testing.T) {
	h := newHarness(t, "reject", true)
	r := mustRequest(t, http.MethodPost, h.proxy.URL+"/loki/api/v1/query_range?limit=7",
		url.Values{"query": {`{app="a"}`}}.Encode(), formContentType)
	if resp := h.do(t, r); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, _, form, _ := h.loki.snapshot()
	// Loki merges body and URL, so both must be present exactly once...
	if got := form["query"]; len(got) != 1 || got[0] != `{app="a", namespace=~"team-a|team-b"}` {
		t.Errorf("query = %q", got)
	}
	if got := form["limit"]; len(got) != 1 || got[0] != "7" {
		t.Errorf("limit = %q", got)
	}
}

func TestNonFormPOSTRejected(t *testing.T) {
	h := newHarness(t, "reject", true)
	r := mustRequest(t, http.MethodPost, h.proxy.URL+"/loki/api/v1/query_range",
		`{"query":"{app=\"a\"}"}`, "application/json")
	resp := h.do(t, r)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415: %s", resp.StatusCode, bodyString(t, resp))
	}
}

func TestUnsupportedMethod(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.do(t, mustRequest(t, http.MethodPut, h.proxy.URL+"/loki/api/v1/query_range", "", ""))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// --- label APIs -------------------------------------------------------------

func TestLabelsScopedWhenNoQuery(t *testing.T) {
	h := newHarness(t, "reject", true)
	if resp := h.get(t, "/loki/api/v1/labels"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _, path, form, _ := h.loki.snapshot()
	if path != "/loki/api/v1/labels" {
		t.Errorf("path = %q", path)
	}
	if got := form.Get("query"); got != enforcedSelector {
		t.Errorf("injected scope = %q", got)
	}
}

func TestLabelsNotScopedWhenDisabled(t *testing.T) {
	h := newHarness(t, "reject", false)
	if resp := h.get(t, "/loki/api/v1/labels"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _, _, form, _ := h.loki.snapshot()
	if got := form.Get("query"); got != "" {
		t.Errorf("expected no injected scope, got %q", got)
	}
}

// A caller-supplied selector is enforced even when enforce_label_apis is off:
// that flag governs injection, never whether user input is enforced.
func TestLabelsCallerQueryAlwaysEnforced(t *testing.T) {
	h := newHarness(t, "reject", false)
	resp := h.get(t, "/loki/api/v1/labels?query="+url.QueryEscape(`{app="api"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, _, form, _ := h.loki.snapshot()
	if got := form.Get("query"); got != `{app="api", namespace=~"team-a|team-b"}` {
		t.Errorf("query = %q", got)
	}
}

func TestLabelValuesEnforcedLabelNeverHitsLoki(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/label/namespace/values")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatalf("Loki must not be asked for an enforced label's values, got %d calls", calls)
	}
	var got labelResponse
	if err := json.Unmarshal([]byte(bodyString(t, resp)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" || strings.Join(got.Data, ",") != "team-a,team-b" {
		t.Errorf("body = %+v", got)
	}
}

func TestLabelValuesOtherLabelScoped(t *testing.T) {
	h := newHarness(t, "reject", true)
	if resp := h.get(t, "/loki/api/v1/label/app/values"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _, path, form, _ := h.loki.snapshot()
	if path != "/loki/api/v1/label/app/values" {
		t.Errorf("path = %q", path)
	}
	if got := form.Get("query"); got != enforcedSelector {
		t.Errorf("scope = %q", got)
	}
}

// --- series -----------------------------------------------------------------

func TestSeriesEveryMatcherEnforced(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/series?match[]="+url.QueryEscape(`{app="a"}`)+
		"&match[]="+url.QueryEscape(`{app="b"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyString(t, resp))
	}
	_, _, _, form, _ := h.loki.snapshot()
	got := form["match[]"]
	want := []string{`{app="a", namespace=~"team-a|team-b"}`, `{app="b", namespace=~"team-a|team-b"}`}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("match[] = %q, want %q", got, want)
	}
}

func TestSeriesOneBadMatcherRejectsWholeRequest(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/series?match[]="+url.QueryEscape(`{app="a"}`)+
		"&match[]="+url.QueryEscape(`{namespace="prod-secret"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatalf("got %d upstream calls", calls)
	}
}

func TestSeriesWithoutMatcherIsScoped(t *testing.T) {
	h := newHarness(t, "reject", true)
	if resp := h.get(t, "/loki/api/v1/series"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _, _, form, _ := h.loki.snapshot()
	if got := form["match[]"]; len(got) != 1 || got[0] != enforcedSelector {
		t.Errorf("match[] = %q", got)
	}
}

// --- allowlist --------------------------------------------------------------

func TestDeniedPaths(t *testing.T) {
	denied := []string{
		"/loki/api/v1/push",
		"/loki/api/v1/delete",
		"/loki/api/v1/",
		"/loki/api/v2/query_range",
		"/api/prom/query",
		"/config",
		"/metrics",
		"/ready",
		"/loki/api/v1/label/app/other",
	}
	for _, p := range denied {
		t.Run(p, func(t *testing.T) {
			h := newHarness(t, "reject", true)
			resp := h.get(t, p)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404", p, resp.StatusCode)
			}
			if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
				t.Errorf("%s reached Loki", p)
			}
		})
	}
}

func TestPushRejectedOverPOST(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.postForm(t, "/loki/api/v1/push", "streams=x")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatal("push reached Loki")
	}
}

func TestTailNotImplemented(t *testing.T) {
	h := newHarness(t, "reject", true)
	resp := h.get(t, "/loki/api/v1/tail?query="+url.QueryEscape(`{app="a"}`))
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatal("tail reached Loki")
	}
}

func TestPassthroughEndpoints(t *testing.T) {
	for _, p := range []string{"/loki/api/v1/status/buildinfo", "/loki/api/v1/format_query"} {
		t.Run(p, func(t *testing.T) {
			h := newHarness(t, "reject", true)
			if resp := h.get(t, p); resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			calls, _, path, _, _ := h.loki.snapshot()
			if calls != 1 || path != p {
				t.Errorf("calls = %d, path = %q", calls, path)
			}
		})
	}
}

// --- credentials ------------------------------------------------------------

func TestClientCredentialsAreNotForwarded(t *testing.T) {
	h := newHarness(t, "reject", true, func(ri *config.ResolvedInstance) {
		ri.Loki.OrgID = "tenant-a"
		ri.Loki.Auth.Token = config.Secret("upstream-token")
	})

	r := mustRequest(t, http.MethodGet,
		h.proxy.URL+"/loki/api/v1/query_range?query="+url.QueryEscape(`{app="a"}`), "", "")
	r.Header.Set("Authorization", "Bearer client-instance-token")
	r.Header.Set("X-Scope-OrgID", "tenant-someone-else")
	if resp := h.do(t, r); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	_, _, _, _, header := h.loki.snapshot()
	if got := header.Get("Authorization"); got != "Bearer upstream-token" {
		t.Errorf("upstream Authorization = %q — the client's own token must never be forwarded", got)
	}
	if got := header.Get("X-Scope-OrgID"); got != "tenant-a" {
		t.Errorf("upstream X-Scope-OrgID = %q — the client must not choose the tenant", got)
	}
}

func TestUpstreamBasicAuth(t *testing.T) {
	h := newHarness(t, "reject", true, func(ri *config.ResolvedInstance) {
		ri.Loki.Auth.Username = "user"
		ri.Loki.Auth.Password = config.Secret("pass")
	})
	if resp := h.get(t, "/loki/api/v1/query_range?query="+url.QueryEscape(`{app="a"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _, _, _, header := h.loki.snapshot()
	r := &http.Request{Header: header}
	u, p, ok := r.BasicAuth()
	if !ok || u != "user" || p != "pass" {
		t.Errorf("upstream basic auth = %q/%q (ok=%t)", u, p, ok)
	}
}

// --- path handling ----------------------------------------------------------

func TestAPISuffix(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/loki/api/v1/query_range", "query_range", true},
		{"/loki/api/v1/label/app/values", "label/app/values", true},
		{"/loki/api/v1//query_range", "query_range", true},
		{"/loki/api/v1/", "", false},
		{"/loki/api/v1", "", false},
		{"/loki/api/v2/query_range", "", false},
		{"/", "", false},
		// Traversal cannot escape the prefix: Clean resolves it first.
		{"/loki/api/v1/../../../push", "", false},
		{"/loki/api/v1/../push", "", false},
		// Percent-encoding is refused outright so an encoded slash cannot
		// forge a route segment.
		{"/loki/api/v1/label/a%2Fb/values", "", false},
		{"/loki/api/v1/%2e%2e/push", "", false},
	}
	for _, tc := range tests {
		got, ok := apiSuffix(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("apiSuffix(%q) = %q, %t; want %q, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLabelValuesTarget(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"label/app/values", "app", true},
		{"label/namespace/values", "namespace", true},
		{"label//values", "", false},
		{"label/a/b/values", "", false},
		{"labels", "", false},
		{"label/app", "", false},
	}
	for _, tc := range tests {
		got, ok := labelValuesTarget(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("labelValuesTarget(%q) = %q, %t; want %q, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	h := newHarness(t, "reject", true)
	big := "query=" + strings.Repeat("a", maxFormBodyBytes+10)
	resp := h.postForm(t, "/loki/api/v1/query_range", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if calls, _, _, _, _ := h.loki.snapshot(); calls != 0 {
		t.Fatal("oversized body reached Loki")
	}
}
