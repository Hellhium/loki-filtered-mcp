# loki-filtered-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) server for [Grafana
Loki](https://grafana.com/oss/loki/), configured by a single YAML file, that
**enforces label matchers on every LogQL query** it sends to Loki. An LLM using
this server can explore and query logs, but only within the scope the operator
declares — it cannot read logs outside the allowed label filters.

It is the union of two ideas:

- a Loki MCP server exposing `loki_query`, `loki_label_names`,
  `loki_label_values` (à la [`scottlepp/loki-mcp`](https://github.com/scottlepp/loki-mcp)), and
- server-side label enforcement on the query AST (à la
  [`prom-label-proxy`](https://github.com/prometheus-community/prom-label-proxy)),
  ported from PromQL to LogQL and driven by YAML instead of CLI flags.

One process serves any number of **instances**. Each instance is an
authenticated scope with its own bearer token(s), its own filters, its own
enforcement policy, and its own choice of surfaces — MCP, a Loki-compatible
read-only HTTP proxy, or both. The token a client presents is what selects the
instance; there are no per-instance URLs to leak.

## How enforcement works

Every query is **parsed into a LogQL AST** using Loki's own parser, and the
configured matchers are injected into **every stream selector** — including
selectors nested inside `rate(...)`, `sum by(...)`, binary operations, `unwrap`
expressions, and multi-selector queries. Enforcement is never string
concatenation, which would be trivial to escape.

**Narrowing within scope is allowed.** A caller may further constrain an
enforced label as long as it stays inside the configured allowed values. With
`namespace` restricted to `team-a` and `team-b`, a query for
`namespace="team-a"` is permitted and returns only `team-a`. Because the
enforced matcher is always AND-ed into the selector, the result can never reach
outside the allowed set — so any caller matcher whose intersection with the
allowed values is non-empty is safe to keep.

The `on_conflict` policy governs only a genuine **conflict** — a matcher on an
enforced label that excludes *every* allowed value (so it can never be satisfied
within scope, e.g. `namespace="prod-secret"`):

| `on_conflict` | Behavior on conflict |
|---------------|----------------------|
| `reject` (default) | The query is refused with an error. Fail closed. |
| `override` | The caller's matcher on that label is dropped and replaced with the enforced one. |

A query that carries no stream selector at all (e.g. a literal like `1 + 1`) is
rejected — the server fails closed rather than forward an unfiltered expression.

### Example

With this instance filter:

```yaml
filters:
  - label: namespace
    values: ["team-a", "team-b"]
```

| Caller sends | Loki receives (`reject`) | Loki receives (`override`) |
|---|---|---|
| `{app="api"}` | `{app="api", namespace=~"team-a\|team-b"}` | same |
| `{app="api"} \|= "error"` | `{app="api", namespace=~"team-a\|team-b"} \|= "error"` | same |
| `sum(rate({app="api"}[5m]))` | `sum(rate({app="api", namespace=~"team-a\|team-b"}[5m]))` | same |
| `{app="api", namespace="team-a"}` | `{app="api", namespace="team-a"}` (narrowing allowed) | same |
| `{namespace="team-b"}` | `{namespace="team-b"}` (narrowing allowed) | same |
| `{namespace="prod-secret"}` | **error** (conflict) | `{namespace=~"team-a\|team-b"}` |

## Endpoints

One listener serves every instance. A request is authenticated **before** it is
routed, so the reply to a bad token never depends on the path asked for.

| Path | Requires | Served when |
|---|---|---|
| `POST /mcp` | `Authorization: Bearer <token>` | the instance has `endpoints.mcp: true` |
| `/loki/api/v1/…` | `Authorization: Bearer <token>` | the instance has `endpoints.proxy: true` |
| `GET /healthz` | nothing | always |

A missing, malformed or unknown token is answered with an identical `401` in
every case, with no `WWW-Authenticate` challenge and nothing naming an
instance. A valid token asking for a surface its instance does not expose gets
`404`.

## Tools

All tools take **only** query parameters — never endpoint, auth, or tenant
arguments. Those come solely from the config, so a client cannot repoint the
server or supply its own credentials.

- **`loki_query`** — run a LogQL query. Params: `query` (required), `start`,
  `end`, `limit`, `format`.
- **`loki_label_names`** — list label names. Params: `start`, `end`, `format`.
  Scoped to the enforced selector when `enforce_label_apis: true`.
- **`loki_label_values`** — list a label's values. Params: `label` (required),
  `start`, `end`, `format`. For an **enforced** label, it returns only the
  configured allowed values and never queries Loki (so it cannot reveal
  out-of-scope namespaces). For other labels it is scoped like `loki_label_names`.

`start`/`end` accept `now`, a relative offset (`-1h`, `-30m`), or RFC3339.
`format` is `raw` (default), `json`, or `text`.

## The Loki proxy

An instance with `endpoints.proxy: true` also serves a **read-only,
Loki-compatible HTTP API** under `/loki/api/v1/`, with the same AST enforcement
applied. This is the [`prom-label-proxy`](https://github.com/prometheus-community/prom-label-proxy)
role, ported to LogQL: point a Grafana Loki datasource or `logcli` at it.

It is a strict allowlist — anything not listed is `404`, including
`/loki/api/v1/push` and every other write path:

| Endpoint | What is enforced |
|---|---|
| `query`, `query_range` | the `query` parameter (required) |
| `index/stats`, `index/volume`, `index/volume_range` | the `query` parameter (required) |
| `labels`, `label/<name>/values` | the `query` parameter if present; otherwise the enforced selector is injected when `enforce_label_apis` is on. For an **enforced** label, the values come straight from config and Loki is never asked. |
| `series` | every `match[]` selector; one is injected when the caller supplies none |
| `status/buildinfo`, `format_query` | nothing to enforce — they carry no log data. Allowlisted so a Grafana datasource saves cleanly and the query editor's prettify works. |
| `tail` | not supported (`501`) — a websocket cannot be enforced by rewriting parameters, so it fails closed |

Parameters are enforced in the URL query **and** in an `application/x-www-form-urlencoded`
POST body, because Grafana sends `query_range` as a form POST. A parameter
supplied in both places is refused as ambiguous (`400`) rather than resolved by
guess. The caller's own `Authorization` and `X-Scope-OrgID` headers stop at the
proxy: upstream credentials and the tenant come only from config.

See [Grafana caveats](#grafana-caveats) below for what this costs in the Grafana UI.

## Configuration

A single YAML file, passed with `-config`. See
[`config.example.yaml`](config.example.yaml) for a fully-commented template.

The top-level `loki`, `enforcement` and `defaults` blocks are **inherited** by
every instance, which overrides only the leaf keys it sets. `filters` is the one
exception: it is never inherited and never merged — each instance declares its
full set, because an implicit change to a filter set is exactly the kind of
silent scope change this project exists to prevent.

| Key | Meaning | Default |
|---|---|---|
| `server.listen` | HTTP listen address for all instances | `:8080` |
| `loki.url` | Loki base URL (http/https) | — (required) |
| `loki.org_id` | Tenant, sent as `X-Scope-OrgID` | unset |
| `loki.auth.username` / `password` | Basic auth **to Loki** | unset |
| `loki.auth.token` | Bearer token **to Loki** (wins over basic auth) | unset |
| `loki.timeout` | Per-request HTTP timeout | `30s` |
| `enforcement.on_conflict` | `reject` or `override` | `reject` |
| `enforcement.enforce_label_apis` | Scope the label APIs to the filters | `true` |
| `defaults.limit` | Default row limit for `loki_query` | `100` |
| `defaults.since` | Default lookback when `start` is omitted | `1h` |

Per instance (`instances[]`), plus an override of any block above:

| Key | Meaning | Default |
|---|---|---|
| `name` | Identifier used in logs and startup errors; never sent to clients | — (required, unique) |
| `auth.type` | Client auth method; only `bearer` is implemented | — (required) |
| `auth.tokens` | Accepted bearer tokens; list two to rotate. Strength is advised, not enforced — use ≥32 random characters | — (≥1 required) |
| `endpoints.mcp` | Expose `POST /mcp` | `true` |
| `endpoints.proxy` | Expose `/loki/api/v1/…` | `false` |
| `filters[].label` | Enforced label name | — (≥1 required) |
| `filters[].values` | Allowed values; 1 → `=`, many → `=~"a\|b"` | — (≥1 required) |

Unknown keys are rejected, so typos fail loudly at startup rather than silently
weakening enforcement. So does anything ambiguous about scope: a token claimed
by two instances, a duplicate instance name, an instance with no filters, or an
instance with both endpoints disabled.

## Running

```bash
make build
./loki-filtered-mcp -config config.example.yaml
```

MCP is served over **Streamable HTTP only** (no SSE, no stdio) at `POST /mcp`,
in stateless mode. Point your MCP client at `http://<listen>/mcp` and give it
the instance's token as an `Authorization: Bearer …` header.

Quick checks:

```bash
# liveness (no token)
curl -s http://localhost:8080/healthz

# MCP handshake
curl -s -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}'

# the proxy, on an instance with endpoints.proxy: true
curl -s -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'query={app="api"}' \
  --get http://localhost:8080/loki/api/v1/query_range
```

### Grafana

Add a Loki datasource with URL `http://<listen>` and, under **Custom HTTP
Headers**, `Authorization: Bearer <instance token>`. Leave the datasource's
tenant field empty — the proxy substitutes the instance's configured `org_id`
and strips whatever the client sent.

#### Grafana caveats

- **`on_conflict: override` is the better fit for proxy instances.** Template
  variables and ad-hoc filters routinely add a matcher on a label you also
  enforce; under `reject` the user gets an opaque 400 whenever the picker
  disagrees with the instance's scope. (Narrowing *within* the allowed values is
  permitted in both modes — `reject` only fires when the picker excludes every
  allowed value.)
- **Live tailing does not work.** `/loki/api/v1/tail` returns `501`.
- **Some query-builder panes stay empty.** `detected_labels`, `detected_fields`
  and `patterns` are not allowlisted: they take a `query` and are enforceable,
  but until that is implemented they are refused rather than passed through
  unenforced, which would leak label and field names from out-of-scope streams.
- **Explore's core works.** Logs volume (`index/volume_range`), the label
  browser (`labels`, `label/<name>/values`) and `series` are all enforced and
  allowlisted.
- **Set `loki.timeout` ≥ the datasource timeout**, or long range queries fail on
  our side first with a less useful error.
- **Server-side access only.** Grafana proxies datasource requests through its
  backend, so no CORS headers are sent. A browser-direct setup will fail — which
  is intended: the token would otherwise sit in the browser.

## Development

```bash
make test        # go test ./...
make vet         # go vet ./...
make fmt-check   # gofmt -l .
make all         # fmt-check + vet + test + build
```

The layout keeps concerns separate so the enforcer is unit-testable without a
live Loki:

- `internal/config` — YAML parsing, instance inheritance, validation, matcher
  precompilation.
- `internal/enforcer` — the LogQL AST enforcement core (the highest-value tests
  live here, including adversarial and corpus cases).
- `internal/auth` — the bearer-token index: token → instance.
- `internal/lokiclient` — the Loki HTTP client.
- `internal/handlers` — the MCP tool handlers for one instance.
- `internal/proxy` — the enforcing Loki-compatible HTTP proxy for one instance.
- `internal/instance` — builds one instance's object graph (enforcer, client,
  MCP server, proxy).
- `internal/router` — the front door: authenticate, then dispatch.
- `internal/e2e` — full-stack integration tests over real HTTP, whose central
  assertion is cross-instance isolation.
- `cmd/server` — the entrypoint.

## Security model

**In scope.** Every LogQL query and every scoped label-API request carries its
instance's configured matchers, enforced on the parsed AST — over MCP and over
the proxy alike. Endpoint, credentials and tenant are fixed by config and never
exposed to clients. Instances share nothing: each has its own enforcer, Loki
client and handlers, so serving the wrong scope is structurally impossible
rather than one missed lookup away.

**Token handling.** Tokens are indexed by SHA-256, so the index holds no
plaintext credential as a key and lookup cost does not vary with a correctly
guessed prefix; a hash hit is confirmed with a constant-time comparison. Tokens
are never logged, never echoed in an error, and never interpolated into a tool
description — an instance is identified by name everywhere. A token claimed by
two instances is refused at startup.

Token **strength is not enforced** — a token is the entire attack surface of its
scope, and choosing it is yours: use at least 32 random characters
(`openssl rand -base64 32 | tr -d '=+/'`). The server only refuses tokens that
cannot work at all: empty, or containing characters invalid in an
`Authorization` header.

**Out of scope.** This server is not a substitute for authentication or
authorization on Loki itself. It trusts its own config and the network path to
Loki. There is no rate limiting, no token expiry, no per-token audit trail, and
tokens are read from the config file only (no files, env vars or secret
managers) — put it behind TLS and your own edge controls as appropriate.
Enforcement constrains **label scope**; it does not limit query cost or
cardinality.
