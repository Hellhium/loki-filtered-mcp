package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
)

// Realistic-length tokens. Length is not enforced (see TestShortTokenIsAccepted).
const (
	tokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadOne(t *testing.T, body string) ResolvedInstance {
	t.Helper()
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Resolved()) != 1 {
		t.Fatalf("want 1 instance, got %d", len(cfg.Resolved()))
	}
	return cfg.Resolved()[0]
}

var goodConfig = fmt.Sprintf(`
server:
  listen: ":9090"
loki:
  url: "https://loki.example.com"
  org_id: "tenant-a"
  auth:
    token: "upstream-secret"
  timeout: 15s
enforcement:
  on_conflict: override
  enforce_label_apis: true
defaults:
  limit: 250
  since: 2h
instances:
  - name: team-a
    auth:
      type: bearer
      tokens: ["%s"]
    endpoints:
      mcp: true
      proxy: true
    filters:
      - label: namespace
        values: ["team-a", "team-b"]
      - label: env
        values: ["prod"]
`, tokenA)

func TestLoadGood(t *testing.T) {
	cfg, err := Load(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}

	ri := cfg.Resolved()[0]
	if ri.Name != "team-a" {
		t.Errorf("name = %q", ri.Name)
	}
	if !ri.MCP || !ri.Proxy {
		t.Errorf("endpoints = mcp:%t proxy:%t", ri.MCP, ri.Proxy)
	}
	if ri.Loki.Timeout.Duration() != 15*time.Second {
		t.Errorf("timeout = %v", ri.Loki.Timeout.Duration())
	}
	if ri.Loki.OrgID != "tenant-a" {
		t.Errorf("org_id = %q", ri.Loki.OrgID)
	}
	if ri.Loki.Auth.Token.Reveal() != "upstream-secret" {
		t.Errorf("upstream token not read back")
	}
	if ri.Defaults.Limit != 250 {
		t.Errorf("limit = %d", ri.Defaults.Limit)
	}
	if ri.Defaults.Since.Duration() != 2*time.Hour {
		t.Errorf("since = %v", ri.Defaults.Since.Duration())
	}
	if ri.Mode() != enforcer.ModeOverride {
		t.Errorf("mode = %v", ri.Mode())
	}
	if len(ri.Tokens) != 1 || ri.Tokens[0].Reveal() != tokenA {
		t.Errorf("tokens = %v", ri.Tokens)
	}

	ms, err := ri.Matchers()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 matchers, got %d", len(ms))
	}
	if got := ms[0].String(); got != `namespace=~"team-a|team-b"` {
		t.Errorf("matcher[0] = %s", got)
	}
	if got := ms[1].String(); got != `env="prod"` {
		t.Errorf("matcher[1] = %s", got)
	}
}

func TestLoadDefaults(t *testing.T) {
	ri := loadOne(t, fmt.Sprintf(`
loki:
  url: "http://localhost:3100"
instances:
  - name: only
    auth:
      type: bearer
      tokens: ["%s"]
    filters:
      - label: namespace
        values: ["team-a"]
`, tokenA))

	if ri.Loki.Timeout.Duration() != 30*time.Second {
		t.Errorf("default timeout = %v", ri.Loki.Timeout.Duration())
	}
	if ri.Defaults.Limit != 100 {
		t.Errorf("default limit = %d", ri.Defaults.Limit)
	}
	if ri.Defaults.Since.Duration() != time.Hour {
		t.Errorf("default since = %v", ri.Defaults.Since.Duration())
	}
	if ri.Enforcement.OnConflict != "reject" {
		t.Errorf("default on_conflict = %q", ri.Enforcement.OnConflict)
	}
	if !ri.Enforcement.EnforceLabelAPIs {
		t.Error("enforce_label_apis must default to true (fail closed)")
	}
	if ri.Mode() != enforcer.ModeReject {
		t.Errorf("default mode = %v", ri.Mode())
	}
	// endpoints omitted → MCP on, proxy off.
	if !ri.MCP || ri.Proxy {
		t.Errorf("default endpoints = mcp:%t proxy:%t, want mcp:true proxy:false", ri.MCP, ri.Proxy)
	}
	// Single value → equality matcher.
	ms, _ := ri.Matchers()
	if got := ms[0].String(); got != `namespace="team-a"` {
		t.Errorf("matcher = %s", got)
	}
}

func TestLoadServerListenDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, fmt.Sprintf(`
loki:
  url: "http://localhost:3100"
instances:
  - name: only
    auth: {type: bearer, tokens: ["%s"]}
    filters: [{label: ns, values: ["a"]}]
`, tokenA)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Errorf("default listen = %q", cfg.Server.Listen)
	}
}

// TestInheritanceAndOverride is the core of the multi-instance schema: global
// blocks supply values, an instance overrides only the leaf keys it sets.
func TestInheritanceAndOverride(t *testing.T) {
	cfg, err := Load(writeConfig(t, fmt.Sprintf(`
loki:
  url: "http://global:3100"
  org_id: "global-tenant"
  auth:
    username: "global-user"
    password: "global-pass"
  timeout: 20s
enforcement:
  on_conflict: reject
  enforce_label_apis: true
defaults:
  limit: 10
  since: 30m
instances:
  - name: inherits
    auth: {type: bearer, tokens: ["%s"]}
    filters: [{label: ns, values: ["a"]}]
  - name: overrides
    auth: {type: bearer, tokens: ["%s"]}
    endpoints: {mcp: false, proxy: true}
    filters: [{label: env, values: ["prod"]}]
    enforcement:
      on_conflict: override
    loki:
      org_id: "own-tenant"
      auth:
        password: "own-pass"
    defaults:
      limit: 999
`, tokenA, tokenB)))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Resolved()[0], cfg.Resolved()[1]

	// Instance A inherits everything.
	if a.Loki.URL != "http://global:3100" || a.Loki.OrgID != "global-tenant" {
		t.Errorf("A loki = %+v", a.Loki)
	}
	if a.Loki.Timeout.Duration() != 20*time.Second {
		t.Errorf("A timeout = %v", a.Loki.Timeout.Duration())
	}
	if a.Enforcement.OnConflict != "reject" || a.Defaults.Limit != 10 {
		t.Errorf("A policy = %+v %+v", a.Enforcement, a.Defaults)
	}

	// Instance B overrides selectively; unset keys still inherit.
	if b.Loki.URL != "http://global:3100" {
		t.Errorf("B url should inherit, got %q", b.Loki.URL)
	}
	if b.Loki.OrgID != "own-tenant" {
		t.Errorf("B org_id = %q", b.Loki.OrgID)
	}
	if b.Loki.Auth.Username != "global-user" {
		t.Errorf("B username should inherit (leaf-key merge), got %q", b.Loki.Auth.Username)
	}
	if b.Loki.Auth.Password.Reveal() != "own-pass" {
		t.Errorf("B password = %q", b.Loki.Auth.Password.Reveal())
	}
	if b.Enforcement.OnConflict != "override" {
		t.Errorf("B on_conflict = %q", b.Enforcement.OnConflict)
	}
	if !b.Enforcement.EnforceLabelAPIs {
		t.Error("B enforce_label_apis should inherit true")
	}
	if b.Defaults.Limit != 999 || b.Defaults.Since.Duration() != 30*time.Minute {
		t.Errorf("B defaults = %+v", b.Defaults)
	}
	if b.MCP || !b.Proxy {
		t.Errorf("B endpoints = mcp:%t proxy:%t", b.MCP, b.Proxy)
	}

	// Filters are never merged: each instance has exactly its own.
	if len(a.Filters) != 1 || a.Filters[0].Label != "ns" {
		t.Errorf("A filters = %+v", a.Filters)
	}
	if len(b.Filters) != 1 || b.Filters[0].Label != "env" {
		t.Errorf("B filters = %+v (must not inherit or merge A's)", b.Filters)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name: "unknown key",
			body: `
loki:
  url: "http://localhost:3100"
  bogus: true
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "field bogus not found",
		},
		{
			name:    "no instances",
			body:    "loki:\n  url: \"http://localhost:3100\"\n",
			wantSub: "at least one instance is required",
		},
		{
			name: "missing url",
			body: `
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "loki.url is required",
		},
		{
			name: "bad url scheme",
			body: `
loki:
  url: "ftp://loki"
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "scheme must be http or https",
		},
		{
			name: "missing name",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "name is required",
		},
		{
			name: "duplicate instance name",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
  - name: a
    auth: {type: bearer, tokens: ["` + tokenB + `"]}
    filters: [{label: ns, values: ["b"]}]
`,
			wantSub: "duplicate instance name",
		},
		{
			name: "duplicate token across instances",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
  - name: b
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["b"]}]
`,
			wantSub: "a token must grant exactly one scope",
		},
		{
			name: "unknown auth type",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: none, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: `auth.type must be "bearer"`,
		},
		{
			name: "no tokens",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: []}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "at least one auth token is required",
		},
		{
			name: "empty token",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: [""]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "must not be empty",
		},
		{
			name: "token with a space",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["aaaaaaaaaaaaaaaa aaaaaaaaaaaaaaaa"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "not valid in an Authorization header",
		},
		{
			name: "no endpoints enabled",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    endpoints: {mcp: false, proxy: false}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "the instance would be unreachable",
		},
		{
			name: "no filters",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
`,
			wantSub: "at least one filter is required",
		},
		{
			name: "empty filter value",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: [""]}]
`,
			wantSub: "must not be empty",
		},
		{
			name: "no filter values",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: []}]
`,
			wantSub: "at least one value is required",
		},
		{
			name: "bad on_conflict",
			body: `
loki: {url: "http://localhost:3100"}
enforcement: {on_conflict: maybe}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "on_conflict must be",
		},
		{
			name: "bad on_conflict on the instance only",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
    enforcement: {on_conflict: sometimes}
`,
			wantSub: "on_conflict must be",
		},
		{
			name: "invalid label name",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: "not a label", values: ["a"]}]
`,
			wantSub: "not a valid label name",
		},
		{
			name: "duplicate filter label",
			body: `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters:
      - {label: ns, values: ["a"]}
      - {label: ns, values: ["b"]}
`,
			wantSub: "duplicate filter",
		},
		{
			name: "bad duration",
			body: `
loki:
  url: "http://localhost:3100"
  timeout: "quickly"
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "invalid duration",
		},
		{
			name: "non-positive limit",
			body: `
loki: {url: "http://localhost:3100"}
defaults: {limit: 0}
instances:
  - name: a
    auth: {type: bearer, tokens: ["` + tokenA + `"]}
    filters: [{label: ns, values: ["a"]}]
`,
			wantSub: "defaults.limit must be positive",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestErrorsNeverEchoTokens: a validation failure must not put the credential
// into a log or a terminal.
func TestErrorsNeverEchoTokens(t *testing.T) {
	const bad = "has a space in it and is otherwise fine"
	_, err := Load(writeConfig(t, `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["`+bad+`"]}
    filters: [{label: ns, values: ["a"]}]
`))
	if err == nil {
		t.Fatal("expected an error for an unusable token")
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("error message leaks the token: %q", err.Error())
	}
}

// Token strength is advised in the example config, not enforced: a short token
// must load, so operators are never blocked during local development.
func TestShortTokenIsAccepted(t *testing.T) {
	ri := loadOne(t, `
loki: {url: "http://localhost:3100"}
instances:
  - name: a
    auth: {type: bearer, tokens: ["short"]}
    filters: [{label: ns, values: ["a"]}]
`)
	if len(ri.Tokens) != 1 || ri.Tokens[0].Reveal() != "short" {
		t.Errorf("tokens = %v", ri.Tokens)
	}
}

func TestSecretRedaction(t *testing.T) {
	s := Secret("super-secret-value")
	if got := s.String(); got != redacted {
		t.Errorf("String() = %q", got)
	}
	for _, format := range []string{"%v", "%s", "%q", "%#v"} {
		if got := fmt.Sprintf(format, s); strings.Contains(got, "super-secret") {
			t.Errorf("%s renders the secret: %q", format, got)
		}
	}
	// A whole struct printed by accident must not leak either.
	ri := ResolvedInstance{Name: "a", Tokens: []Secret{s}, Loki: Loki{Auth: Auth{Token: s}}}
	if got := fmt.Sprintf("%+v", ri); strings.Contains(got, "super-secret") {
		t.Errorf("%%+v of a ResolvedInstance leaks a token: %q", got)
	}
	if Secret("").String() != "" {
		t.Error("an unset secret should render empty, not [redacted]")
	}
	if s.Reveal() != "super-secret-value" {
		t.Error("Reveal must return the plaintext")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFilterValues(t *testing.T) {
	ri := ResolvedInstance{Filters: []Filter{{Label: "ns", Values: []string{"a", "b"}}}}
	if v, ok := ri.FilterValues("ns"); !ok || len(v) != 2 {
		t.Errorf("FilterValues(ns) = %v, %t", v, ok)
	}
	if _, ok := ri.FilterValues("app"); ok {
		t.Error("FilterValues(app) should report not enforced")
	}
}
