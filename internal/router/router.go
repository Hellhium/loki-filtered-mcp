// Package router is the front door: one listener serving every instance.
//
// A request is authenticated *before* it is routed. The bearer token is the
// only thing that selects an instance — there are no per-instance URLs — so a
// leaked endpoint address reveals nothing, and the response to a bad token is
// identical whatever path was requested.
package router

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Hellhium/loki-filtered-mcp/internal/auth"
	"github.com/Hellhium/loki-filtered-mcp/internal/instance"
)

// Paths served. Everything else is 404.
const (
	// HealthPath is the only unauthenticated path. It reports liveness and
	// nothing about the configuration.
	HealthPath = "/healthz"
	// MCPPath is the Streamable HTTP MCP endpoint.
	MCPPath = "/mcp"
	// ProxyPrefix is the root of the Loki-compatible read API.
	ProxyPrefix = "/loki/"
)

// Router dispatches authenticated requests to their instance's surfaces.
type Router struct {
	index *auth.Index[*instance.Instance]
}

// New builds the router and its token index. It fails if two instances claim
// the same token.
func New(instances []*instance.Instance) (*Router, error) {
	entries := make([]auth.Entry[*instance.Instance], 0, len(instances))
	for _, in := range instances {
		tokens := make([]string, 0, len(in.Config.Tokens))
		for _, t := range in.Config.Tokens {
			tokens = append(tokens, t.Reveal())
		}
		entries = append(entries, auth.Entry[*instance.Instance]{
			Name:   in.Name,
			Tokens: tokens,
			Value:  in,
		})
	}
	index, err := auth.NewIndex(entries)
	if err != nil {
		return nil, err
	}
	return &Router{index: index}, nil
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == HealthPath {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	// Authenticate first: the reply to an unknown token must not depend on
	// which path was asked for, or it would map the server's topology.
	token, ok := auth.BearerToken(r)
	if !ok {
		unauthorized(w)
		return
	}
	in, ok := rt.index.Lookup(token)
	if !ok {
		unauthorized(w)
		return
	}

	switch {
	case r.URL.Path == MCPPath:
		if in.MCP == nil {
			notFound(w)
			return
		}
		in.MCP.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, ProxyPrefix):
		if in.Proxy == nil {
			notFound(w)
			return
		}
		in.Proxy.ServeHTTP(w, r)
	default:
		notFound(w)
	}
}

// unauthorized answers identically for a missing header, a wrong scheme and an
// unknown token, and names no instance. No WWW-Authenticate challenge is sent:
// there is no interactive auth flow to discover.
func unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, "unauthorized")
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, "unknown endpoint")
}

func writeJSON(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": message})
}
