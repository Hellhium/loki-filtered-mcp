package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/instance"
)

const (
	tokenBoth  = "both-both-both-both-both-both-bo"
	tokenMCP   = "mcponly-mcponly-mcponly-mcponly-"
	tokenProxy = "proxyonly-proxyonly-proxyonly-pr"
	tokenNope  = "unknown-unknown-unknown-unknown-"
)

func testRouter(t *testing.T) *Router {
	t.Helper()

	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	}))
	t.Cleanup(loki.Close)

	base := func(name, token string, mcp, proxy bool) config.ResolvedInstance {
		return config.ResolvedInstance{
			Name:        name,
			Tokens:      []config.Secret{config.Secret(token)},
			MCP:         mcp,
			Proxy:       proxy,
			Filters:     []config.Filter{{Label: "namespace", Values: []string{name}}},
			Enforcement: config.Enforcement{OnConflict: "reject", EnforceLabelAPIs: true},
			Loki:        config.Loki{URL: loki.URL, Timeout: config.Duration(0)},
			Defaults:    config.Defaults{Limit: 100},
		}
	}

	instances, err := instance.BuildAll([]config.ResolvedInstance{
		base("both", tokenBoth, true, true),
		base("mcponly", tokenMCP, true, false),
		base("proxyonly", tokenProxy, false, true),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(instances)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func do(t *testing.T, rt *Router, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	return w
}

func TestHealthIsUnauthenticated(t *testing.T) {
	rt := testRouter(t)
	w := do(t, rt, http.MethodGet, HealthPath, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if body, _ := io.ReadAll(w.Body); string(body) != "ok\n" {
		t.Errorf("body = %q", body)
	}
}

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	rt := testRouter(t)
	paths := []string{MCPPath, "/loki/api/v1/labels", "/nonexistent"}
	tokens := []string{"", tokenNope}

	var bodies []string
	for _, p := range paths {
		for _, tok := range tokens {
			w := do(t, rt, http.MethodGet, p, tok)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s with token %q: status = %d, want 401", p, tok, w.Code)
			}
			bodies = append(bodies, w.Body.String())
		}
	}
	// The reply must not vary with the path or with why authentication failed —
	// otherwise it maps which endpoints and instances exist.
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("401 bodies differ: %q vs %q", bodies[0], bodies[i])
		}
	}
}

func TestNoWWWAuthenticateChallenge(t *testing.T) {
	rt := testRouter(t)
	w := do(t, rt, http.MethodGet, MCPPath, "")
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none", got)
	}
}

func TestDisabledEndpointsAre404(t *testing.T) {
	rt := testRouter(t)

	// The MCP-only instance has no proxy surface.
	if w := do(t, rt, http.MethodGet, "/loki/api/v1/labels", tokenMCP); w.Code != http.StatusNotFound {
		t.Errorf("proxy for an mcp-only instance: status = %d, want 404", w.Code)
	}
	// The proxy-only instance has no MCP surface.
	if w := do(t, rt, http.MethodPost, MCPPath, tokenProxy); w.Code != http.StatusNotFound {
		t.Errorf("mcp for a proxy-only instance: status = %d, want 404", w.Code)
	}
	// The instance with both reaches both.
	if w := do(t, rt, http.MethodGet, "/loki/api/v1/labels", tokenBoth); w.Code != http.StatusOK {
		t.Errorf("proxy for a both-instance: status = %d, want 200 (%s)", w.Code, w.Body)
	}
}

func TestUnknownPathAfterAuth(t *testing.T) {
	rt := testRouter(t)
	w := do(t, rt, http.MethodGet, "/admin", tokenBoth)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestNewRejectsSharedToken(t *testing.T) {
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(loki.Close)

	ri := config.ResolvedInstance{
		Tokens:      []config.Secret{config.Secret(tokenBoth)},
		MCP:         true,
		Filters:     []config.Filter{{Label: "ns", Values: []string{"a"}}},
		Enforcement: config.Enforcement{OnConflict: "reject"},
		Loki:        config.Loki{URL: loki.URL},
		Defaults:    config.Defaults{Limit: 100},
	}
	a, b := ri, ri
	a.Name, b.Name = "a", "b"

	instances, err := instance.BuildAll([]config.ResolvedInstance{a, b}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(instances); err == nil {
		t.Fatal("expected an error when two instances share a token")
	}
}
