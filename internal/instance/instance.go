// Package instance builds the runtime object graph for one configured
// instance: its enforcer, its Loki client, and whichever surfaces it exposes.
//
// Each instance gets its own enforcer, its own Loki client and its own MCP
// server. Nothing is shared between instances, so serving the wrong scope is
// not one missed lookup away — it is structurally impossible.
package instance

import (
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
	"github.com/Hellhium/loki-filtered-mcp/internal/handlers"
	"github.com/Hellhium/loki-filtered-mcp/internal/lokiclient"
	"github.com/Hellhium/loki-filtered-mcp/internal/proxy"
)

// Instance is one authenticated scope, ready to serve.
type Instance struct {
	// Name identifies the instance in logs. It is never sent to clients.
	Name string

	// Config is the resolved configuration this instance was built from.
	Config config.ResolvedInstance

	// Enforcer is the LogQL enforcer shared by this instance's surfaces.
	Enforcer *enforcer.Enforcer

	// MCP serves Streamable HTTP MCP, or is nil when endpoints.mcp is false.
	MCP http.Handler

	// Proxy serves the enforced Loki read API, or is nil when
	// endpoints.proxy is false.
	Proxy http.Handler
}

// Build constructs one instance. version is reported in the MCP handshake.
func Build(ri config.ResolvedInstance, version string) (*Instance, error) {
	filters, err := ri.EnforcedFilters()
	if err != nil {
		return nil, fmt.Errorf("instance %q: %w", ri.Name, err)
	}
	enf := enforcer.New(filters, ri.Mode())
	cli := lokiclient.New(ri.Loki)

	in := &Instance{Name: ri.Name, Config: ri, Enforcer: enf}

	if ri.MCP {
		h := handlers.New(ri, enf, cli)
		s := server.NewMCPServer(
			// Deliberately the product name, not the instance name: a client
			// learns nothing about the server's topology from the handshake.
			"loki-filtered-mcp", version,
			server.WithToolCapabilities(false),
			server.WithRecovery(),
		)
		s.AddTool(h.QueryTool(), h.HandleQuery)
		s.AddTool(h.LabelNamesTool(), h.HandleLabelNames)
		s.AddTool(h.LabelValuesTool(), h.HandleLabelValues)

		// Stateless: every request is independent and carries no per-session
		// state, which suits a filtering proxy and avoids session-ID
		// bookkeeping for clients. The endpoint path is set for parity with the
		// router's mount point; ServeHTTP itself does not route on it.
		in.MCP = server.NewStreamableHTTPServer(s,
			server.WithEndpointPath("/mcp"),
			server.WithStateLess(true),
		)
	}

	if ri.Proxy {
		px, err := proxy.New(ri, enf)
		if err != nil {
			return nil, fmt.Errorf("instance %q: %w", ri.Name, err)
		}
		in.Proxy = px
	}

	return in, nil
}

// BuildAll constructs every instance, in config order.
func BuildAll(ris []config.ResolvedInstance, version string) ([]*Instance, error) {
	out := make([]*Instance, 0, len(ris))
	for _, ri := range ris {
		in, err := Build(ri, version)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

// Endpoints renders the enabled surfaces for a startup log line.
func (in *Instance) Endpoints() string {
	switch {
	case in.MCP != nil && in.Proxy != nil:
		return "mcp+proxy"
	case in.MCP != nil:
		return "mcp"
	case in.Proxy != nil:
		return "proxy"
	default:
		return "none"
	}
}
