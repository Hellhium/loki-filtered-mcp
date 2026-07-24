// Package e2e drives the full stack — router, per-instance auth, MCP server,
// enforcer, Loki client and the enforcing proxy — over real HTTP against a fake
// Loki. Its central assertion is cross-instance isolation: nothing a client of
// one instance can send may produce a request carrying another instance's
// filters or tenant.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/instance"
	"github.com/Hellhium/loki-filtered-mcp/internal/router"
)

const (
	tokenA = "team-a-token-team-a-token-team-a"
	tokenB = "team-b-token-team-b-token-team-b"
	badTok = "not-a-token-not-a-token-not-a-to"
)

// lokiRequest is one request that reached the upstream.
type lokiRequest struct {
	Path  string
	Query string
	Match []string
	Org   string
}

// fakeLoki records every request it serves.
type fakeLoki struct {
	mu   sync.Mutex
	reqs []lokiRequest
}

func (f *fakeLoki) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.reqs = append(f.reqs, lokiRequest{
			Path:  r.URL.Path,
			Query: r.Form.Get("query"),
			Match: r.Form["match[]"],
			Org:   r.Header.Get("X-Scope-OrgID"),
		})
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/label/"), strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`{"status":"success","data":["app","namespace"]}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"app":"foo"},"values":[["0","hello"]]}]}}`))
		}
	}
}

func (f *fakeLoki) all() []lokiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lokiRequest(nil), f.reqs...)
}

func (f *fakeLoki) last() (lokiRequest, bool) {
	all := f.all()
	if len(all) == 0 {
		return lokiRequest{}, false
	}
	return all[len(all)-1], true
}

func (f *fakeLoki) count() int { return len(f.all()) }

// setup builds a two-instance server: team-a (reject) and team-b (override,
// two filters), both exposing MCP and the proxy, each pinned to its own tenant.
func setup(t *testing.T) (*httptest.Server, *fakeLoki) {
	t.Helper()

	loki := &fakeLoki{}
	lokiSrv := httptest.NewServer(loki.handler())
	t.Cleanup(lokiSrv.Close)

	instances, err := instance.BuildAll([]config.ResolvedInstance{
		{
			Name:        "team-a",
			Tokens:      []config.Secret{config.Secret(tokenA)},
			MCP:         true,
			Proxy:       true,
			Filters:     []config.Filter{{Label: "namespace", Values: []string{"team-a"}}},
			Enforcement: config.Enforcement{OnConflict: "reject", EnforceLabelAPIs: true},
			Loki:        config.Loki{URL: lokiSrv.URL, OrgID: "tenant-a", Timeout: config.Duration(0)},
			Defaults:    config.Defaults{Limit: 100},
		},
		{
			Name:   "team-b",
			Tokens: []config.Secret{config.Secret(tokenB)},
			MCP:    true,
			Proxy:  true,
			Filters: []config.Filter{
				{Label: "namespace", Values: []string{"team-b"}},
				{Label: "env", Values: []string{"prod"}},
			},
			Enforcement: config.Enforcement{OnConflict: "override", EnforceLabelAPIs: true},
			Loki:        config.Loki{URL: lokiSrv.URL, OrgID: "tenant-b", Timeout: config.Duration(0)},
			Defaults:    config.Defaults{Limit: 100},
		},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	rt, err := router.New(instances)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv, loki
}

// --- MCP helpers ------------------------------------------------------------

func mcpClient(t *testing.T, srv *httptest.Server, token string) *client.Client {
	t.Helper()
	mc, err := client.NewStreamableHttpClient(srv.URL+router.MCPPath,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() })

	ctx := context.Background()
	if err := mc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}
	return mc
}

func callTool(t *testing.T, mc *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := mc.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// --- HTTP helpers -----------------------------------------------------------

func httpGet(t *testing.T, srv *httptest.Server, path, token string) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// --- MCP surface ------------------------------------------------------------

func TestE2EToolsListed(t *testing.T) {
	srv, _ := setup(t)
	mc := mcpClient(t, srv, tokenA)
	tools, err := mc.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"loki_query", "loki_label_names", "loki_label_values"} {
		if !names[want] {
			t.Errorf("tool %q not advertised", want)
		}
	}
}

func TestE2EMCPUnauthenticated(t *testing.T) {
	srv, loki := setup(t)
	mc, err := client.NewStreamableHttpClient(srv.URL+router.MCPPath,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + badTok}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mc.Close() })

	ctx := context.Background()
	_ = mc.Start(ctx)
	if _, err := mc.Initialize(ctx, mcp.InitializeRequest{}); err == nil {
		t.Fatal("expected the MCP handshake to fail with an unknown token")
	}
	if loki.count() != 0 {
		t.Fatalf("an unauthenticated client reached Loki (%d calls)", loki.count())
	}
}

func TestE2EPerInstanceEnforcement(t *testing.T) {
	srv, loki := setup(t)

	// team-a: one filter, equality matcher.
	res := callTool(t, mcpClient(t, srv, tokenA), "loki_query", map[string]any{"query": `{app="foo"}`})
	if res.IsError {
		t.Fatalf("team-a query failed: %s", resultText(t, res))
	}
	got, _ := loki.last()
	if got.Query != `{app="foo", namespace="team-a"}` {
		t.Errorf("team-a query = %q", got.Query)
	}
	if got.Org != "tenant-a" {
		t.Errorf("team-a org = %q", got.Org)
	}

	// team-b: two filters, its own tenant.
	res = callTool(t, mcpClient(t, srv, tokenB), "loki_query", map[string]any{"query": `{app="foo"}`})
	if res.IsError {
		t.Fatalf("team-b query failed: %s", resultText(t, res))
	}
	got, _ = loki.last()
	if got.Query != `{app="foo", namespace="team-b", env="prod"}` {
		t.Errorf("team-b query = %q", got.Query)
	}
	if got.Org != "tenant-b" {
		t.Errorf("team-b org = %q", got.Org)
	}
}

// TestE2ECrossInstanceIsolation is the invariant the whole design exists for.
func TestE2ECrossInstanceIsolation(t *testing.T) {
	srv, loki := setup(t)
	a := mcpClient(t, srv, tokenA)

	// Every way team-a's client might try to reach team-b's logs.
	attempts := []string{
		`{namespace="team-b"}`,
		`{namespace=~".*"}`,
		`{namespace!="team-a"}`,
		`{app="foo", namespace="team-b"}`,
		`sum(rate({namespace="team-b"}[5m]))`,
		`{namespace="team-a"} or {namespace="team-b"}`,
	}
	for _, q := range attempts {
		res := callTool(t, a, "loki_query", map[string]any{"query": q})
		if res.IsError {
			continue // rejected outright — also fine
		}
		got, ok := loki.last()
		if !ok {
			continue
		}
		if strings.Contains(got.Query, "team-b") {
			t.Errorf("query %q produced an upstream query reaching team-b: %q", q, got.Query)
		}
	}

	// And nothing team-a sent may have carried team-b's tenant.
	for _, r := range loki.all() {
		if r.Org != "tenant-a" {
			t.Errorf("team-a produced a request for tenant %q: %+v", r.Org, r)
		}
		if !strings.Contains(r.Query, `namespace="team-a"`) && r.Query != "" {
			t.Errorf("team-a produced an unscoped query: %q", r.Query)
		}
	}
}

func TestE2ERejectVsOverride(t *testing.T) {
	srv, loki := setup(t)

	// team-a is in reject mode: the request never reaches Loki.
	before := loki.count()
	res := callTool(t, mcpClient(t, srv, tokenA), "loki_query", map[string]any{"query": `{namespace="team-b"}`})
	if !res.IsError {
		t.Fatalf("expected a rejection, got: %s", resultText(t, res))
	}
	if loki.count() != before {
		t.Fatal("Loki must not be called on a rejected query")
	}

	// team-b is in override mode: the query is corrected, not refused.
	res = callTool(t, mcpClient(t, srv, tokenB), "loki_query", map[string]any{"query": `{namespace="team-a"}`})
	if res.IsError {
		t.Fatalf("override mode should not refuse: %s", resultText(t, res))
	}
	got, _ := loki.last()
	if got.Query != `{namespace="team-b", env="prod"}` {
		t.Errorf("overridden query = %q", got.Query)
	}
}

func TestE2ELabelValuesPerInstance(t *testing.T) {
	srv, loki := setup(t)

	before := loki.count()
	res := callTool(t, mcpClient(t, srv, tokenA), "loki_label_values", map[string]any{"label": "namespace"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	if loki.count() != before {
		t.Fatal("an enforced label's values must not be fetched from Loki")
	}
	txt := resultText(t, res)
	if !strings.Contains(txt, "team-a") || strings.Contains(txt, "team-b") {
		t.Errorf("team-a saw %q — it must list only its own values", txt)
	}

	res = callTool(t, mcpClient(t, srv, tokenB), "loki_label_values", map[string]any{"label": "namespace"})
	txt = resultText(t, res)
	if !strings.Contains(txt, "team-b") || strings.Contains(txt, "team-a") {
		t.Errorf("team-b saw %q", txt)
	}
}

func TestE2ELabelNamesScoped(t *testing.T) {
	srv, loki := setup(t)
	res := callTool(t, mcpClient(t, srv, tokenA), "loki_label_names", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	got, _ := loki.last()
	if !strings.HasSuffix(got.Path, "/labels") {
		t.Errorf("path = %q", got.Path)
	}
	if got.Query != `{namespace="team-a"}` {
		t.Errorf("scope query = %q", got.Query)
	}
}

// --- proxy surface ----------------------------------------------------------

func TestE2EProxyEnforced(t *testing.T) {
	srv, loki := setup(t)
	path := "/loki/api/v1/query_range?query=" + url.QueryEscape(`{app="foo"}`)

	resp, body := httpGet(t, srv, path, tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	got, _ := loki.last()
	if got.Query != `{app="foo", namespace="team-a"}` {
		t.Errorf("proxy query = %q", got.Query)
	}
	if got.Org != "tenant-a" {
		t.Errorf("proxy org = %q", got.Org)
	}

	// The same URL under team-b's token yields team-b's scope and tenant.
	resp, body = httpGet(t, srv, path, tokenB)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	got, _ = loki.last()
	if got.Query != `{app="foo", namespace="team-b", env="prod"}` {
		t.Errorf("proxy query = %q", got.Query)
	}
	if got.Org != "tenant-b" {
		t.Errorf("proxy org = %q", got.Org)
	}
}

func TestE2EProxyUnauthenticated(t *testing.T) {
	srv, loki := setup(t)
	for _, tok := range []string{"", badTok} {
		resp, _ := httpGet(t, srv, "/loki/api/v1/labels", tok)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", tok, resp.StatusCode)
		}
	}
	if loki.count() != 0 {
		t.Fatalf("unauthenticated proxy requests reached Loki (%d)", loki.count())
	}
}

func TestE2EProxyLabelValuesPerInstance(t *testing.T) {
	srv, loki := setup(t)
	before := loki.count()

	resp, body := httpGet(t, srv, "/loki/api/v1/label/namespace/values", tokenA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if loki.count() != before {
		t.Fatal("an enforced label's values must not be fetched from Loki")
	}
	var got struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Data[0] != "team-a" {
		t.Errorf("data = %v", got.Data)
	}
}

func TestE2EProxyWriteDenied(t *testing.T) {
	srv, loki := setup(t)
	resp, _ := httpGet(t, srv, "/loki/api/v1/push", tokenA)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if loki.count() != 0 {
		t.Fatal("push reached Loki")
	}
}

func TestE2EHealth(t *testing.T) {
	srv, _ := setup(t)
	resp, body := httpGet(t, srv, router.HealthPath, "")
	if resp.StatusCode != http.StatusOK || body != "ok\n" {
		t.Errorf("health = %d %q", resp.StatusCode, body)
	}
}
