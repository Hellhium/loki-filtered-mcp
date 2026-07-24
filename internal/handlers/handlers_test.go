package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
	"github.com/Hellhium/loki-filtered-mcp/internal/lokiclient"
)

// testHandlers builds Handlers wired to a fake Loki server. The captured
// *string is updated with the `query` param of the last request Loki received.
func testHandlers(t *testing.T, inst *config.ResolvedInstance, body string, lastQuery *string, lastPath *string) *Handlers {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastQuery != nil {
			*lastQuery = r.URL.Query().Get("query")
		}
		if lastPath != nil {
			*lastPath = r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	inst.Loki.URL = srv.URL
	fs, err := inst.EnforcedFilters()
	if err != nil {
		t.Fatal(err)
	}
	enf := enforcer.New(fs, inst.Mode())
	cli := lokiclient.New(inst.Loki)
	return New(*inst, enf, cli)
}

func req(args map[string]any) mcp.CallToolRequest {
	var r mcp.CallToolRequest
	r.Params.Arguments = args
	return r
}

func mustText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("no content in result")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content is not text: %T", res.Content[0])
	}
	return tc.Text
}

func baseInstance(onConflict string, enforceLabelAPIs bool) *config.ResolvedInstance {
	return &config.ResolvedInstance{
		Name: "test",
		Loki: config.Loki{Timeout: config.Duration(0)},
		Filters: []config.Filter{
			{Label: "namespace", Values: []string{"team-a", "team-b"}},
		},
		Enforcement: config.Enforcement{OnConflict: onConflict, EnforceLabelAPIs: enforceLabelAPIs},
		Defaults:    config.Defaults{Limit: 100, Since: config.Duration(0)},
	}
}

func TestQueryEnforcedReachesLoki(t *testing.T) {
	var gotQuery string
	inst := baseInstance("reject", true)
	h := testHandlers(t, inst,
		`{"status":"success","data":{"resultType":"streams","result":[]}}`,
		&gotQuery, nil)

	res, err := h.HandleQuery(context.Background(), req(map[string]any{"query": `{app="foo"}`}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", mustText(t, res))
	}
	// The query Loki sees must carry the enforced matcher.
	if gotQuery != `{app="foo", namespace=~"team-a|team-b"}` {
		t.Fatalf("query reaching Loki = %q", gotQuery)
	}
}

func TestQueryRejectModeConflictNoLokiCall(t *testing.T) {
	var gotQuery string
	called := false
	inst := baseInstance("reject", true)
	// Wrap to detect any call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotQuery = r.URL.Query().Get("query")
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	inst.Loki.URL = srv.URL
	fs, _ := inst.EnforcedFilters()
	h := New(*inst, enforcer.New(fs, inst.Mode()), lokiclient.New(inst.Loki))

	res, err := h.HandleQuery(context.Background(), req(map[string]any{"query": `{namespace="evil"}`}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error for conflicting query, got: %s", mustText(t, res))
	}
	if called {
		t.Fatalf("Loki must not be called on a rejected query (got query %q)", gotQuery)
	}
	if !strings.Contains(mustText(t, res), "rejected") {
		t.Errorf("error message = %q", mustText(t, res))
	}
}

func TestQueryOverrideModeRewrites(t *testing.T) {
	var gotQuery string
	inst := baseInstance("override", true)
	h := testHandlers(t, inst, `{"status":"success","data":{"result":[]}}`, &gotQuery, nil)

	res, err := h.HandleQuery(context.Background(), req(map[string]any{"query": `{namespace="evil"}`}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", mustText(t, res))
	}
	if gotQuery != `{namespace=~"team-a|team-b"}` {
		t.Fatalf("override query = %q", gotQuery)
	}
}

func TestQueryInvalidLogQL(t *testing.T) {
	inst := baseInstance("reject", true)
	h := testHandlers(t, inst, `{}`, nil, nil)
	res, _ := h.HandleQuery(context.Background(), req(map[string]any{"query": `{app=}`}))
	if !res.IsError {
		t.Fatal("expected error for invalid LogQL")
	}
	if !strings.Contains(mustText(t, res), "invalid LogQL") {
		t.Errorf("message = %q", mustText(t, res))
	}
}

func TestLabelValuesEnforcedLabelShortCircuits(t *testing.T) {
	var gotPath string
	called := false
	inst := baseInstance("reject", true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotPath = r.URL.Path
		w.Write([]byte(`{"status":"success","data":["team-a","team-b","team-secret"]}`))
	}))
	t.Cleanup(srv.Close)
	inst.Loki.URL = srv.URL
	fs, _ := inst.EnforcedFilters()
	h := New(*inst, enforcer.New(fs, inst.Mode()), lokiclient.New(inst.Loki))

	res, err := h.HandleLabelValues(context.Background(), req(map[string]any{"label": "namespace"}))
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatalf("Loki must not be queried for an enforced label's values (path %q)", gotPath)
	}
	got := mustText(t, res)
	if !strings.Contains(got, "team-a") || !strings.Contains(got, "team-b") {
		t.Errorf("expected configured values, got %q", got)
	}
	if strings.Contains(got, "team-secret") {
		t.Errorf("must not leak Loki's real value set: %q", got)
	}
}

func TestLabelValuesNonEnforcedLabelScoped(t *testing.T) {
	var gotQuery, gotPath string
	inst := baseInstance("reject", true) // enforce_label_apis = true
	h := testHandlers(t, inst, `{"status":"success","data":["a","b"]}`, &gotQuery, &gotPath)

	res, err := h.HandleLabelValues(context.Background(), req(map[string]any{"label": "app"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", mustText(t, res))
	}
	if gotPath != "/loki/api/v1/label/app/values" {
		t.Errorf("path = %q", gotPath)
	}
	// Scoped by the enforced selector.
	if gotQuery != `{namespace=~"team-a|team-b"}` {
		t.Errorf("scope query = %q", gotQuery)
	}
}

func TestLabelNamesScopingToggle(t *testing.T) {
	// enforce_label_apis = false → no scope query.
	var gotQuery string
	inst := baseInstance("reject", false)
	h := testHandlers(t, inst, `{"status":"success","data":["app","namespace"]}`, &gotQuery, nil)
	if _, err := h.HandleLabelNames(context.Background(), req(map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "" {
		t.Errorf("expected no scope query when enforce_label_apis is false, got %q", gotQuery)
	}

	// enforce_label_apis = true → scoped.
	var gotQuery2 string
	inst2 := baseInstance("reject", true)
	h2 := testHandlers(t, inst2, `{"status":"success","data":["app","namespace"]}`, &gotQuery2, nil)
	if _, err := h2.HandleLabelNames(context.Background(), req(map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if gotQuery2 != `{namespace=~"team-a|team-b"}` {
		t.Errorf("expected scoped label_names, got %q", gotQuery2)
	}
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestParseTime(t *testing.T) {
	now := mustParseRFC3339(t, "2026-07-24T12:00:00Z")
	tests := []struct {
		in   string
		want string
	}{
		{"now", "2026-07-24T12:00:00Z"},
		{"-1h", "2026-07-24T11:00:00Z"},
		{"2026-07-24T06:30:00Z", "2026-07-24T06:30:00Z"},
	}
	for _, tc := range tests {
		got, err := parseTime(tc.in, now)
		if err != nil {
			t.Fatalf("parseTime(%q): %v", tc.in, err)
		}
		if got.UTC().Format("2006-01-02T15:04:05Z07:00") != tc.want {
			t.Errorf("parseTime(%q) = %v, want %s", tc.in, got.UTC(), tc.want)
		}
	}
	if _, err := parseTime("garbage", now); err == nil {
		t.Error("expected error for garbage time")
	}
}

func TestFormatQueryResultsRaw(t *testing.T) {
	res := &lokiclient.QueryResult{}
	res.Data.Result = []lokiclient.StreamEntry{
		{Stream: map[string]string{"app": "x", "namespace": "team-a"}, Values: [][]string{{"0", "hello"}}},
	}
	out, err := formatQueryResults(res, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "app=x") {
		t.Errorf("raw output = %q", out)
	}
}
