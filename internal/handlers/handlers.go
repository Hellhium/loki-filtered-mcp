// Package handlers implements the MCP tools. Every LogQL query is run through
// the enforcer before it reaches Loki, and the tools deliberately expose no
// endpoint/auth/tenant arguments — those come only from config.
//
// The one config value that ever reaches a client is the enforced scope — the
// filter labels and their allowed values — and only when
// enforcement.disclose_filters is on. Nothing else is interpolated into the
// handshake instructions or a tool description: not the upstream URL, the
// tenant, any credential, the instance name, nor the existence of any other
// instance (the reference server leaked credentials into descriptions this way).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
	"github.com/Hellhium/loki-filtered-mcp/internal/lokiclient"
)

// Handlers holds the dependencies shared by every tool of ONE instance. It is
// built once per instance at startup and injected — no globals, no env vars,
// and no shared state between instances.
type Handlers struct {
	inst config.ResolvedInstance
	enf  *enforcer.Enforcer
	cli  *lokiclient.Client

	// scopeSelector is the enforced stream selector passed to Loki's label
	// APIs, or "" when enforcement.enforce_label_apis is false.
	scopeSelector string

	// disclosed is the enforced selector rendered for the client, or "" when
	// enforcement.disclose_filters is off. It gates every mention of the scope
	// in the instructions and the tool descriptions; it never gates
	// enforcement itself, which is unconditional.
	disclosed string
}

// New wires the handlers for a single resolved instance.
func New(inst config.ResolvedInstance, enf *enforcer.Enforcer, cli *lokiclient.Client) *Handlers {
	h := &Handlers{inst: inst, enf: enf, cli: cli}
	if inst.Enforcement.EnforceLabelAPIs {
		h.scopeSelector = enf.Selector()
	}
	if inst.Enforcement.DiscloseFilters {
		h.disclosed = enf.Selector()
	}
	return h
}

// --- scope disclosure -------------------------------------------------------

// Instructions returns the MCP handshake instructions for this instance: a
// plain statement of the scope every query is confined to, so a client writes
// queries that fit it instead of discovering the boundary by being rejected.
//
// It is empty when enforcement.disclose_filters is off, in which case the
// handshake says nothing about the filter set — the queries are enforced just
// the same.
func (h *Handlers) Instructions() string {
	if h.disclosed == "" {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("This server exposes a filtered view of Loki. Every LogQL query you send is parsed and rewritten so that every stream selector carries the enforced matchers ")
	sb.WriteString(h.disclosed)
	sb.WriteString(".\n\nEnforced labels, and the only values reachable through this server:\n")
	for _, f := range h.inst.Filters {
		sb.WriteString("  - " + f.Label + ": " + strings.Join(f.Values, ", ") + "\n")
	}

	sb.WriteString("\nYou may narrow within those values — a matcher like ")
	sb.WriteString(exampleNarrowing(h.inst.Filters))
	sb.WriteString(" is kept as you wrote it. ")
	if h.inst.Enforcement.OnConflict == "override" {
		sb.WriteString("A matcher on an enforced label that excludes every value above is dropped and replaced by the enforced one, so the results you get back stay in scope even when your matcher did not.")
	} else {
		sb.WriteString("A matcher on an enforced label that excludes every value above is a conflict: the query is rejected and nothing is sent to Loki. Rewrite it to stay within the values above.")
	}

	sb.WriteString("\n\nloki_label_values on an enforced label returns exactly the values listed above. ")
	if h.inst.Enforcement.EnforceLabelAPIs {
		sb.WriteString("Every other label listing is scoped to the same selector, so it describes only streams inside this scope. ")
	}
	sb.WriteString("Logs outside this scope cannot be reached from here, and no tool argument changes the scope, the endpoint, the credentials or the tenant.")

	return sb.String()
}

// scopeSuffix is the sentence appended to a tool description when the scope is
// disclosed, so the scope is visible to clients that ignore instructions.
func (h *Handlers) scopeSuffix() string {
	if h.disclosed == "" {
		return ""
	}
	return " Enforced scope: every stream selector is rewritten to carry " + h.disclosed + "."
}

// exampleNarrowing renders a concrete in-scope matcher from the first filter,
// e.g. `namespace="team-a"`. Filters are validated as non-empty upstream.
func exampleNarrowing(filters []config.Filter) string {
	for _, f := range filters {
		if len(f.Values) > 0 {
			return f.Label + `="` + f.Values[0] + `"`
		}
	}
	return "one on an enforced label"
}

// --- tool definitions -------------------------------------------------------

// QueryTool defines loki_query.
func (h *Handlers) QueryTool() mcp.Tool {
	return mcp.NewTool("loki_query",
		mcp.WithDescription("Run a LogQL query against Loki. Configured label filters are enforced automatically; you cannot query outside the allowed scope."+h.scopeSuffix()),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("query", mcp.Required(), mcp.Description("LogQL query string")),
		mcp.WithString("start", mcp.Description("Start time: 'now', a relative offset like '-1h', or RFC3339 (default: configured lookback before end)")),
		mcp.WithString("end", mcp.Description("End time: 'now', a relative offset, or RFC3339 (default: now)")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of entries to return")),
		mcp.WithString("format", mcp.Description("Output format: raw, json, or text (default: raw)"), mcp.DefaultString("raw")),
	)
}

// LabelNamesTool defines loki_label_names.
func (h *Handlers) LabelNamesTool() mcp.Tool {
	return mcp.NewTool("loki_label_names",
		mcp.WithDescription("List label names from Loki, scoped to the enforced filters."+h.scopeSuffix()),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Description("Start time (default: configured lookback before end)")),
		mcp.WithString("end", mcp.Description("End time (default: now)")),
		mcp.WithString("format", mcp.Description("Output format: raw, json, or text (default: raw)"), mcp.DefaultString("raw")),
	)
}

// LabelValuesTool defines loki_label_values.
func (h *Handlers) LabelValuesTool() mcp.Tool {
	return mcp.NewTool("loki_label_values",
		mcp.WithDescription("List values for a label from Loki, scoped to the enforced filters. For an enforced label, only the allowed values are returned."+h.scopeSuffix()),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("label", mcp.Required(), mcp.Description("Label name to list values for")),
		mcp.WithString("start", mcp.Description("Start time (default: configured lookback before end)")),
		mcp.WithString("end", mcp.Description("End time (default: now)")),
		mcp.WithString("format", mcp.Description("Output format: raw, json, or text (default: raw)"), mcp.DefaultString("raw")),
	)
}

// --- tool handlers ----------------------------------------------------------

// HandleQuery enforces the query and runs it against Loki.
func (h *Handlers) HandleQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}

	enforced, err := h.enf.Enforce(query)
	if err != nil {
		// Enforcement failures (parse errors, conflicts) are user errors: report
		// them to the LLM as a tool error rather than failing the RPC. No request
		// is made to Loki.
		return mcp.NewToolResultError(enforcementErrorMessage(err)), nil
	}

	start, end, err := h.timeRange(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit := req.GetInt("limit", h.inst.Defaults.Limit)

	res, err := h.cli.QueryRange(ctx, enforced, start, end, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
	}

	out, err := formatQueryResults(res, format(req))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

// HandleLabelNames lists label names, scoped when enforce_label_apis is set.
func (h *Handlers) HandleLabelNames(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, end, err := h.timeRange(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	res, err := h.cli.LabelNames(ctx, start, end, h.scopeSelector)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("labels query failed: %v", err)), nil
	}
	return mcp.NewToolResultText(formatLabelList(res.Data, format(req), "labels")), nil
}

// HandleLabelValues lists values for a label. For an enforced label it never
// asks Loki — it returns exactly the configured allowed values.
func (h *Handlers) HandleLabelValues(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	label, err := req.RequireString("label")
	if err != nil {
		return mcp.NewToolResultError("label is required"), nil
	}

	// Short-circuit enforced labels: revealing the full value set of a label we
	// restrict would leak the existence of out-of-scope streams.
	if values, ok := h.inst.FilterValues(label); ok {
		sorted := append([]string(nil), values...)
		sort.Strings(sorted)
		return mcp.NewToolResultText(formatLabelList(sorted, format(req), "values for label '"+label+"'")), nil
	}

	start, end, err := h.timeRange(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	res, err := h.cli.LabelValues(ctx, label, start, end, h.scopeSelector)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("label values query failed: %v", err)), nil
	}
	return mcp.NewToolResultText(formatLabelList(res.Data, format(req), "values for label '"+label+"'")), nil
}

// --- helpers ----------------------------------------------------------------

func enforcementErrorMessage(err error) string {
	switch {
	case errors.Is(err, enforcer.ErrConflict):
		return "query rejected: " + err.Error()
	case errors.Is(err, enforcer.ErrParse):
		return "invalid LogQL: " + err.Error()
	case errors.Is(err, enforcer.ErrNoSelector):
		return "query rejected: " + err.Error()
	default:
		return err.Error()
	}
}

func format(req mcp.CallToolRequest) string {
	return req.GetString("format", "raw")
}

// timeRange resolves start/end (unix seconds) from the request, applying the
// configured lookback default. end defaults to now; start defaults to
// end - defaults.since.
func (h *Handlers) timeRange(req mcp.CallToolRequest) (start, end int64, err error) {
	now := time.Now()

	endT := now
	if s := req.GetString("end", ""); s != "" {
		endT, err = parseTime(s, now)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid end time: %v", err)
		}
	}

	startT := endT.Add(-h.inst.Defaults.Since.Duration())
	if s := req.GetString("start", ""); s != "" {
		startT, err = parseTime(s, now)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start time: %v", err)
		}
	}

	return startT.Unix(), endT.Unix(), nil
}

// parseTime accepts "now", relative offsets ("-1h", "-30m"), RFC3339, and a few
// common layouts. now is passed in so the function is deterministic in tests.
func parseTime(s string, now time.Time) (time.Time, error) {
	if s == "now" {
		return now, nil
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		if d, err := time.ParseDuration(s); err == nil {
			return now.Add(d), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format: %s", s)
}

// formatQueryResults renders a query_range response in the requested format.
func formatQueryResults(res *lokiclient.QueryResult, format string) (string, error) {
	if len(res.Data.Result) == 0 {
		if format == "json" {
			return `{"message": "No logs found matching the query"}`, nil
		}
		return "No logs found matching the query", nil
	}

	switch format {
	case "json":
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal JSON: %v", err)
		}
		return string(b), nil

	case "raw":
		var sb strings.Builder
		for _, entry := range res.Data.Result {
			labels := formatStreamLabels(entry.Stream)
			for _, val := range entry.Values {
				if len(val) >= 2 {
					sb.WriteString(fmt.Sprintf("%s %s%s\n", formatTimestamp(val[0]), labels, val[1]))
				}
			}
		}
		return sb.String(), nil

	case "text":
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d streams:\n\n", len(res.Data.Result)))
		for i, entry := range res.Data.Result {
			sb.WriteString(fmt.Sprintf("Stream %s%d:\n", formatStreamLabels(entry.Stream), i+1))
			for _, val := range entry.Values {
				if len(val) >= 2 {
					sb.WriteString(fmt.Sprintf("[%s] %s\n", formatTimestamp(val[0]), val[1]))
				}
			}
			sb.WriteString("\n")
		}
		return sb.String(), nil

	default:
		return "", fmt.Errorf("unsupported format: %s. Supported formats: raw, json, text", format)
	}
}

func formatStreamLabels(stream map[string]string) string {
	if len(stream) == 0 {
		return ""
	}
	keys := make([]string, 0, len(stream))
	for k := range stream {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, stream[k]))
	}
	return "{" + strings.Join(parts, ",") + "} "
}

func formatTimestamp(raw string) string {
	ns, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return raw
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

// formatLabelList renders a flat list of labels/values. noun is used in the
// empty and text-mode headers, e.g. "labels" or "values for label 'env'".
func formatLabelList(data []string, format, noun string) string {
	if len(data) == 0 {
		if format == "json" {
			return fmt.Sprintf(`{"message": "No %s found"}`, noun)
		}
		return "No " + noun + " found"
	}

	switch format {
	case "json":
		b, _ := json.MarshalIndent(map[string]any{"status": "success", "data": data}, "", "  ")
		return string(b)
	case "text":
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d %s:\n\n", len(data), noun))
		for i, v := range data {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, v))
		}
		return sb.String()
	default: // raw
		return strings.Join(data, "\n") + "\n"
	}
}
