# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project goal

**`loki-filtered-mcp`** — a **Loki MCP server** that is configured by a **simple YAML config file**
and that **enforces query filters** (label matchers) on every LogQL query it sends to Loki.

Conceptually this is the union of two existing projects (see `references/` below):

- A Loki MCP server exposing tools like `loki_query`, `loki_label_names`, `loki_label_values`
  over MCP (stdio + HTTP/SSE), à la `scottlepp/loki-mcp`.
- Server-side label/matcher enforcement on the query AST, à la `prom-label-proxy` — except
  applied to **LogQL** instead of PromQL, and driven by the YAML config instead of CLI flags.

The distinguishing feature versus the reference MCP server: an operator writes a YAML config
declaring the Loki endpoint(s) and a set of **filters** (label matchers) that are transparently
injected into user queries, so the LLM cannot read logs outside the allowed scope.

## `references/` — read-only, gitignored

`references/` is in `.gitignore` and is **not** part of this project. It holds two upstream
clones kept purely as design references. Read them for patterns; never import from them or edit
them.

- `references/loki-mcp` (`github.com/scottlepp/loki-mcp`) — Go MCP server for Loki using
  `github.com/mark3labs/mcp-go`. Key files:
  - `cmd/server/main.go` — registers tools, serves MCP over stdio and over HTTP with both a
    legacy SSE endpoint (`/sse` + `/mcp`) and a Streamable HTTP server on one port (`PORT`, default 8080).
  - `internal/handlers/loki.go` — tool definitions and handlers; builds `loki/api/v1/query_range`
    and `label`/`label/<name>/values` URLs, sets `X-Scope-OrgID`, basic-auth or bearer token,
    formats results. Config today is via env vars (`LOKI_URL`, `LOKI_ORG_ID`, `LOKI_USERNAME`,
    `LOKI_PASSWORD`, `LOKI_TOKEN`) — **this is what we replace with a YAML config**.
- `references/prom-label-proxy` (`github.com/prometheus-community/prom-label-proxy`) — the
  filter-enforcement pattern. Key files:
  - `injectproxy/enforce.go` — `PromQLEnforcer.Enforce(query)` parses the query and walks the AST
    (`EnforceNode`) injecting/overriding label matchers. This is the model to port to LogQL.
  - `injectproxy/routes.go` — different enforcer sources (static value, HTTP header, HTTP form).
    Our filters come from YAML, so a **static/config-driven enforcer** is the closest analog.

## Architecture the code should follow

- **Language/toolchain: Go** (references target Go 1.24). MCP via `github.com/mark3labs/mcp-go`.
- **Transport: HTTP MCP.** Serve MCP over HTTP (Streamable HTTP; SSE optional for legacy clients),
  not stdio. See `references/loki-mcp/cmd/server/main.go` for the multiplexed HTTP setup.
- **LogQL filter enforcement is the core invariant.** Enforce by parsing the LogQL query and
  injecting matchers into the AST (using Loki's LogQL parser, `github.com/grafana/loki/.../logql`),
  **not** by string concatenation — string munging is unsafe and defeats the security purpose.
  If a user query already constrains an enforced label, the behavior is **configurable via YAML**:
  either **override** the user's matcher with the enforced one, or **reject** the query with an
  error (prom-label-proxy's `errorOnReplace` option is the precedent).
- **Config is the source of truth.** A single YAML file defines Loki endpoint(s), auth,
  tenant/org, and the filter set. Parse it once at startup into a typed struct; pass it (not env
  vars or globals) into handlers. Prefer `gopkg.in/yaml.v3`.
- **Separation of concerns:** keep config parsing, the LogQL enforcer, the Loki HTTP client, and
  the MCP tool handlers in distinct packages so the enforcer can be unit-tested without a live Loki.

## Commands

No build tooling exists in this repo yet (only `.gitignore`). When Go code is added, expect the
standard flow (mirrors the reference projects):

```bash
go build ./...           # build
go test ./...            # run all tests
go test ./path/to/pkg -run TestName   # run a single test
go vet ./...             # vet
gofmt -l .               # list unformatted files
```

The reference `loki-mcp` also ships a `Makefile`, `docker-compose.yml` (local Loki + Grafana),
and helper scripts (`run-mcp-server.sh`, `insert-loki-logs.sh`, `test-loki-query.sh`) — useful
templates for a local dev/test loop against a real Loki instance.

## Testing focus

The highest-value tests are **table-driven enforcer tests**: given a config filter set and an
input LogQL query, assert the exact enforced query — including adversarial inputs that try to
widen scope, use vector/range aggregations, or restate an enforced label. See
`references/prom-label-proxy/injectproxy/enforce_test.go` for the pattern to adapt.
