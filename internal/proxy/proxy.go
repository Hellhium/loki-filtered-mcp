// Package proxy exposes a Loki-compatible read API for one instance, with the
// same LogQL enforcement the MCP tools apply. It is what lets a Grafana
// datasource or logcli point at loki-filtered-mcp directly.
//
// The design is a strict allowlist, not a passthrough: only the endpoints in
// the route table below reach Loki, everything else is 404 — including
// /loki/api/v1/push. Enforcement happens in this handler, *before* any upstream
// connection is opened, so a rejected query never touches Loki.
//
// Two details matter more than they look:
//
//   - Parameters are enforced in the URL query *and* in a form POST body.
//     Grafana sends query_range as an urlencoded POST, so a proxy that only
//     rewrote the URL query string would be bypassed by moving the parameter
//     into the body.
//   - The client's own Authorization header is consumed by the router and never
//     forwarded; upstream credentials and the tenant come only from config.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
	"github.com/Hellhium/loki-filtered-mcp/internal/lokiclient"
)

// maxFormBodyBytes caps the request body we are willing to parse and rewrite.
// Loki query parameters are small; anything larger is not a legitimate query.
const maxFormBodyBytes = 1 << 20 // 1 MiB

const formContentType = "application/x-www-form-urlencoded"

// Proxy serves the enforced Loki read API for a single instance.
type Proxy struct {
	inst config.ResolvedInstance
	enf  *enforcer.Enforcer
	rp   *httputil.ReverseProxy
}

type targetKey struct{}

// New builds the proxy handler for one resolved instance.
func New(inst config.ResolvedInstance, enf *enforcer.Enforcer) (*Proxy, error) {
	// Validate the upstream once at startup rather than per request.
	if _, err := lokiclient.APIURL(inst.Loki.URL, "query_range"); err != nil {
		return nil, err
	}

	// Clone the default transport so proxy env vars and sane connection pooling
	// still apply, then bound how long Loki may take to start replying. A
	// whole-request deadline is deliberately not imposed: a legitimate range
	// query can stream results for a long time.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = inst.Loki.Timeout.Duration()

	p := &Proxy{inst: inst, enf: enf}
	p.rp = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		ErrorHandler: p.upstreamError,
		Transport:    tr,
	}
	return p, nil
}

// rewrite points the outbound request at Loki and swaps the credentials. The
// target URL was computed (and enforced) by ServeHTTP.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	target, _ := pr.In.Context().Value(targetKey{}).(*url.URL)
	if target == nil {
		// Unreachable: ServeHTTP always stores a target before proxying.
		return
	}
	pr.SetXForwarded()
	pr.Out.URL = target
	pr.Out.Host = target.Host

	// The caller's credentials and tenant claims stop here.
	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del("X-Scope-OrgID")

	if tok := p.inst.Loki.Auth.Token.Reveal(); tok != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+tok)
	} else if u, pw := p.inst.Loki.Auth.Username, p.inst.Loki.Auth.Password.Reveal(); u != "" || pw != "" {
		pr.Out.SetBasicAuth(u, pw)
	}
	if p.inst.Loki.OrgID != "" {
		pr.Out.Header.Set("X-Scope-OrgID", p.inst.Loki.OrgID)
	}
}

func (p *Proxy) upstreamError(w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream Loki request failed: %v", err))
}

// action is what enforcement a route requires.
type action int

const (
	// actQuery enforces a mandatory `query` parameter.
	actQuery action = iota
	// actQueryOptional enforces `query` when present; when absent it injects the
	// enforced selector if enforce_label_apis is on. Used for the label APIs.
	actQueryOptional
	// actMatch enforces every `match[]` selector, injecting one when absent.
	actMatch
	// actPassthrough forwards a request that carries no LogQL at all.
	actPassthrough
)

// ServeHTTP routes, enforces, then proxies.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only GET and POST are supported")
		return
	}

	suffix, ok := apiSuffix(r.URL.EscapedPath())
	if !ok {
		writeError(w, http.StatusNotFound, "unknown endpoint")
		return
	}

	// Enforced-label short-circuit: never ask Loki for the values of a label we
	// restrict — the answer would reveal out-of-scope streams exist.
	if label, isValues := labelValuesTarget(suffix); isValues {
		if values, enforced := p.inst.FilterValues(label); enforced {
			writeJSON(w, http.StatusOK, labelResponse{Status: "success", Data: sorted(values)})
			return
		}
	}

	act, ok := routeAction(suffix)
	if !ok {
		if suffix == "tail" {
			// A websocket upgrade cannot be enforced by rewriting parameters;
			// fail closed rather than proxy an unenforced stream.
			writeError(w, http.StatusNotImplemented, "live tailing is not supported by this proxy")
			return
		}
		writeError(w, http.StatusNotFound, "unknown endpoint")
		return
	}

	vals, inBody, err := p.requestParams(r)
	if err != nil {
		writeHTTPError(w, err)
		return
	}

	if err := p.enforceParams(act, vals); err != nil {
		writeHTTPError(w, err)
		return
	}

	target, err := lokiclient.APIURL(p.inst.Loki.URL, suffix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid upstream configuration")
		return
	}

	// Re-emit every parameter on one side. Loki merges the URL query and the
	// form body, so consolidating is equivalent — and it guarantees no
	// un-enforced copy of a parameter survives on the other side.
	encoded := vals.Encode()
	if inBody {
		target.RawQuery = ""
		r.Body = io.NopCloser(strings.NewReader(encoded))
		r.ContentLength = int64(len(encoded))
		r.Header.Del("Content-Length") // the transport writes it from ContentLength
		r.Header.Set("Content-Type", formContentType)
	} else {
		target.RawQuery = encoded
		r.Body = http.NoBody
		r.ContentLength = 0
	}

	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, target)))
}

// enforceParams applies the route's enforcement to the parameter set in place.
func (p *Proxy) enforceParams(act action, vals url.Values) error {
	switch act {
	case actQuery:
		return p.enforceQuery(vals, true)
	case actQueryOptional:
		return p.enforceQuery(vals, false)
	case actMatch:
		return p.enforceMatch(vals)
	case actPassthrough:
		return nil
	default:
		return &httpError{http.StatusInternalServerError, "unhandled route"}
	}
}

// enforceQuery rewrites the `query` parameter through the enforcer.
func (p *Proxy) enforceQuery(vals url.Values, required bool) error {
	if len(vals["query"]) > 1 {
		return &httpError{http.StatusBadRequest, `parameter "query" was supplied more than once`}
	}
	q := vals.Get("query")
	if q == "" {
		if required {
			return &httpError{http.StatusBadRequest, `missing required parameter "query"`}
		}
		// Label APIs with no selector: scope them when configured to.
		if p.inst.Enforcement.EnforceLabelAPIs {
			vals.Set("query", p.enf.Selector())
		}
		return nil
	}
	enforced, err := p.enf.Enforce(q)
	if err != nil {
		return enforcementError(err)
	}
	vals.Set("query", enforced)
	return nil
}

// enforceMatch rewrites every selector in `match[]` (and the `match` alias).
// A caller-supplied selector is always enforced, regardless of
// enforce_label_apis — that flag only governs whether we *inject* scope where
// the caller supplied none.
func (p *Proxy) enforceMatch(vals url.Values) error {
	found := false
	for _, key := range []string{"match[]", "match"} {
		ms := vals[key]
		if len(ms) == 0 {
			continue
		}
		found = true
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			enforced, err := p.enf.Enforce(m)
			if err != nil {
				return enforcementError(err)
			}
			out = append(out, enforced)
		}
		vals[key] = out
	}
	if !found {
		// Loki needs at least one selector here; supply the enforced scope so an
		// unqualified /series can never enumerate out-of-scope streams.
		vals.Set("match[]", p.enf.Selector())
	}
	return nil
}

// requestParams collects the request parameters from the URL query and, for a
// form POST, from the body. inBody reports whether the parameters should be
// written back into the body.
func (p *Proxy) requestParams(r *http.Request) (url.Values, bool, error) {
	urlVals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "malformed query string"}
	}

	if r.Method != http.MethodPost {
		return urlVals, false, nil
	}

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		// A POST with no body behaves like a GET with query parameters.
		return urlVals, false, nil
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "malformed Content-Type header"}
	}
	if mediaType != formContentType {
		return nil, false, &httpError{http.StatusUnsupportedMediaType,
			"request body must be " + formContentType}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxFormBodyBytes+1))
	if err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "could not read request body"}
	}
	if len(body) > maxFormBodyBytes {
		return nil, false, &httpError{http.StatusRequestEntityTooLarge, "request body is too large"}
	}
	bodyVals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "malformed form body"}
	}

	// A parameter split across the URL and the body has ambiguous precedence —
	// exactly the kind of ambiguity an enforcing proxy must not resolve by guess.
	merged := make(url.Values, len(urlVals)+len(bodyVals))
	for k, v := range urlVals {
		merged[k] = append([]string(nil), v...)
	}
	for k, v := range bodyVals {
		if _, dup := merged[k]; dup {
			return nil, false, &httpError{http.StatusBadRequest,
				fmt.Sprintf("parameter %q was supplied in both the URL and the request body", k)}
		}
		merged[k] = append([]string(nil), v...)
	}
	return merged, true, nil
}

// --- routing ----------------------------------------------------------------

// routeAction maps an allowlisted API suffix to its enforcement. Anything not
// listed here is a 404 — including /push and every write path.
func routeAction(suffix string) (action, bool) {
	switch suffix {
	case "query_range", "query", "index/stats", "index/volume", "index/volume_range":
		return actQuery, true
	case "labels":
		return actQueryOptional, true
	case "series":
		return actMatch, true
	case "status/buildinfo", "format_query":
		// Carry no log data: buildinfo takes no parameters, format_query only
		// echoes back the caller's own query string. Allowlisted so a Grafana
		// datasource can save and so the query editor's prettify works.
		return actPassthrough, true
	}
	if _, ok := labelValuesTarget(suffix); ok {
		return actQueryOptional, true
	}
	return 0, false
}

// labelValuesTarget reports whether suffix is label/<name>/values, and the name.
func labelValuesTarget(suffix string) (string, bool) {
	rest, ok := strings.CutPrefix(suffix, "label/")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, "/values")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// apiSuffix validates that the request path is under /loki/api/v1/ and returns
// the part after it. The path is cleaned first so no amount of ".." can escape
// the prefix, and percent-encoding is refused outright: no legitimate Loki API
// path needs it, and allowing it would let an encoded slash forge a route.
func apiSuffix(escapedPath string) (string, bool) {
	cleaned := path.Clean(escapedPath)
	suffix, ok := strings.CutPrefix(cleaned, lokiclient.APIPrefix+"/")
	if !ok || suffix == "" {
		return "", false
	}
	if strings.Contains(suffix, "%") {
		return "", false
	}
	return suffix, true
}

// --- responses --------------------------------------------------------------

// httpError is a caller-facing failure with the status to report it under.
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }

// enforcementError maps an enforcer failure to a 400 with the same wording the
// MCP tools use, so both surfaces explain a rejection identically.
func enforcementError(err error) error {
	switch {
	case errors.Is(err, enforcer.ErrConflict), errors.Is(err, enforcer.ErrNoSelector):
		return &httpError{http.StatusBadRequest, "query rejected: " + err.Error()}
	case errors.Is(err, enforcer.ErrParse):
		return &httpError{http.StatusBadRequest, "invalid LogQL: " + err.Error()}
	default:
		return &httpError{http.StatusBadRequest, err.Error()}
	}
}

type labelResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

type errorResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func writeHTTPError(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeError(w, he.status, he.message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal error")
}

// writeError replies in Loki's error shape so Grafana surfaces the message.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Status: "error", ErrorType: "bad_data", Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
