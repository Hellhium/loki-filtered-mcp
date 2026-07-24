package instance

import (
	"testing"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
)

func base(name string, mcp, proxy bool, values ...string) config.ResolvedInstance {
	if len(values) == 0 {
		values = []string{name}
	}
	return config.ResolvedInstance{
		Name:        name,
		MCP:         mcp,
		Proxy:       proxy,
		Filters:     []config.Filter{{Label: "namespace", Values: values}},
		Enforcement: config.Enforcement{OnConflict: "reject", EnforceLabelAPIs: true},
		Loki:        config.Loki{URL: "http://loki:3100", Timeout: config.Duration(0)},
		Defaults:    config.Defaults{Limit: 100},
	}
}

func TestBuildEndpointGating(t *testing.T) {
	tests := []struct {
		name          string
		mcp, proxy    bool
		wantEndpoints string
	}{
		{"both", true, true, "mcp+proxy"},
		{"mcp only", true, false, "mcp"},
		{"proxy only", false, true, "proxy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, err := Build(base("x", tc.mcp, tc.proxy), "test")
			if err != nil {
				t.Fatal(err)
			}
			if (in.MCP != nil) != tc.mcp {
				t.Errorf("MCP handler present = %t, want %t", in.MCP != nil, tc.mcp)
			}
			if (in.Proxy != nil) != tc.proxy {
				t.Errorf("Proxy handler present = %t, want %t", in.Proxy != nil, tc.proxy)
			}
			if in.Endpoints() != tc.wantEndpoints {
				t.Errorf("Endpoints() = %q, want %q", in.Endpoints(), tc.wantEndpoints)
			}
		})
	}
}

// TestInstancesAreIndependent: two instances must share no enforcer, no handler
// and no scope. Shared state here would be a cross-tenant leak.
func TestInstancesAreIndependent(t *testing.T) {
	instances, err := BuildAll([]config.ResolvedInstance{
		base("team-a", true, true),
		base("team-b", true, true),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, b := instances[0], instances[1]

	if a.Enforcer == b.Enforcer {
		t.Fatal("instances share an enforcer")
	}
	if a.MCP == b.MCP || a.Proxy == b.Proxy {
		t.Fatal("instances share a handler")
	}

	// Each enforcer carries only its own scope.
	if got := a.Enforcer.Selector(); got != `{namespace="team-a"}` {
		t.Errorf("team-a selector = %q", got)
	}
	if got := b.Enforcer.Selector(); got != `{namespace="team-b"}` {
		t.Errorf("team-b selector = %q", got)
	}

	gotA, err := a.Enforcer.Enforce(`{app="x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if gotA != `{app="x", namespace="team-a"}` {
		t.Errorf("team-a enforced %q", gotA)
	}
	if _, err := a.Enforcer.Enforce(`{namespace="team-b"}`); err == nil {
		t.Error("team-a's enforcer accepted team-b's scope")
	}
}

func TestBuildRejectsBadUpstream(t *testing.T) {
	ri := base("x", false, true)
	ri.Loki.URL = "://not a url"
	if _, err := Build(ri, "test"); err == nil {
		t.Fatal("expected an error for an unparseable upstream URL")
	}
}
