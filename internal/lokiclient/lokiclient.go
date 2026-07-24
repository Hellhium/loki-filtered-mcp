// Package lokiclient is a thin HTTP client for the Loki query and label APIs.
// Endpoint, auth and tenant come only from config — never from tool arguments —
// so callers cannot repoint the server or change credentials. Query scoping is
// the caller's responsibility (see the enforcer / handlers packages); this
// client passes the query strings it is given straight through.
package lokiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
)

// Client talks to a single Loki endpoint using credentials fixed at construction.
type Client struct {
	baseURL  string
	orgID    string
	username string
	password string
	token    string
	http     *http.Client
}

// New builds a Client from the Loki config section.
func New(cfg config.Loki) *Client {
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:  cfg.URL,
		orgID:    cfg.OrgID,
		username: cfg.Auth.Username,
		password: cfg.Auth.Password.Reveal(),
		token:    cfg.Auth.Token.Reveal(),
		http:     &http.Client{Timeout: timeout},
	}
}

// QueryResult is the response of the query_range endpoint.
type QueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string        `json:"resultType"`
		Result     []StreamEntry `json:"result"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// StreamEntry is one stream and its log lines from a query_range response.
type StreamEntry struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"` // [timestamp_ns, line]
}

// LabelResult is the response shape shared by the labels and label-values APIs.
type LabelResult struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
	Error  string   `json:"error,omitempty"`
}

// QueryRange runs a LogQL query over [start, end] (unix seconds) with a row limit.
func (c *Client) QueryRange(ctx context.Context, query string, start, end int64, limit int) (*QueryResult, error) {
	u, err := c.buildURL("query_range")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start, 10))
	q.Set("end", strconv.FormatInt(end, 10))
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()

	var out QueryResult
	if err := c.do(ctx, u.String(), &out); err != nil {
		return nil, err
	}
	if out.Status == "error" {
		return nil, fmt.Errorf("loki error: %s", out.Error)
	}
	return &out, nil
}

// LabelNames lists label names. scopeQuery, when non-empty, is passed as the
// `query` stream-selector parameter so the result is restricted to matching
// streams.
func (c *Client) LabelNames(ctx context.Context, start, end int64, scopeQuery string) (*LabelResult, error) {
	u, err := c.buildURL("labels")
	if err != nil {
		return nil, err
	}
	return c.labelRequest(ctx, u, start, end, scopeQuery)
}

// LabelValues lists the values of a single label, optionally scoped by scopeQuery.
func (c *Client) LabelValues(ctx context.Context, label string, start, end int64, scopeQuery string) (*LabelResult, error) {
	u, err := c.buildURL("label/" + url.PathEscape(label) + "/values")
	if err != nil {
		return nil, err
	}
	return c.labelRequest(ctx, u, start, end, scopeQuery)
}

func (c *Client) labelRequest(ctx context.Context, u *url.URL, start, end int64, scopeQuery string) (*LabelResult, error) {
	q := u.Query()
	q.Set("start", strconv.FormatInt(start, 10))
	q.Set("end", strconv.FormatInt(end, 10))
	if scopeQuery != "" {
		q.Set("query", scopeQuery)
	}
	u.RawQuery = q.Encode()

	var out LabelResult
	if err := c.do(ctx, u.String(), &out); err != nil {
		return nil, err
	}
	if out.Status == "error" {
		return nil, fmt.Errorf("loki error: %s", out.Error)
	}
	return &out, nil
}

// APIPrefix is the Loki v1 API path prefix, without a trailing slash.
const APIPrefix = "/loki/api/v1"

// APIURL resolves an API path (e.g. "query_range", "label/app/values") under a
// Loki base URL, tolerating base URLs that already contain the /loki/api/v1
// prefix. It is shared by the typed client and the reverse proxy so both derive
// upstream URLs identically.
func APIURL(baseURL, apiPath string) (*url.URL, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid loki url: %w", err)
	}
	base := strings.TrimRight(u.Path, "/")
	if !strings.Contains(base, "loki/api/v1") {
		base += APIPrefix
	}
	u.Path = base + "/" + strings.TrimPrefix(apiPath, "/")
	return u, nil
}

// buildURL resolves an API path under this client's base URL.
func (c *Client) buildURL(apiPath string) (*url.URL, error) {
	return APIURL(c.baseURL, apiPath)
}

func (c *Client) do(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	// Bearer token wins over basic auth (mirrors the reference client).
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if c.orgID != "" {
		req.Header.Set("X-Scope-OrgID", c.orgID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loki HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode loki response: %w", err)
	}
	return nil
}
