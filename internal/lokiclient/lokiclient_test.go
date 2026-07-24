package lokiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Hellhium/loki-filtered-mcp/internal/config"
)

// capture records what the fake Loki received.
type capture struct {
	path        string
	query       string
	start       string
	end         string
	limit       string
	authHeader  string
	basicUser   string
	basicPass   string
	basicOK     bool
	orgID       string
	queryValues map[string][]string
}

func newFakeLoki(t *testing.T, status int, body string, cap *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.queryValues = r.URL.Query()
		cap.query = r.URL.Query().Get("query")
		cap.start = r.URL.Query().Get("start")
		cap.end = r.URL.Query().Get("end")
		cap.limit = r.URL.Query().Get("limit")
		cap.authHeader = r.Header.Get("Authorization")
		cap.orgID = r.Header.Get("X-Scope-OrgID")
		cap.basicUser, cap.basicPass, cap.basicOK = r.BasicAuth()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func lokiCfg(url string) config.Loki {
	return config.Loki{URL: url, Timeout: config.Duration(0)}
}

func TestQueryRange(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK,
		`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"app":"x"},"values":[["1","hello"]]}]}}`,
		cap)
	defer srv.Close()

	c := New(lokiCfg(srv.URL))
	res, err := c.QueryRange(context.Background(), `{app="x", ns="a"}`, 100, 200, 42)
	if err != nil {
		t.Fatal(err)
	}
	if cap.path != "/loki/api/v1/query_range" {
		t.Errorf("path = %q", cap.path)
	}
	if cap.query != `{app="x", ns="a"}` {
		t.Errorf("query = %q", cap.query)
	}
	if cap.start != "100" || cap.end != "200" || cap.limit != "42" {
		t.Errorf("params start=%s end=%s limit=%s", cap.start, cap.end, cap.limit)
	}
	if len(res.Data.Result) != 1 || res.Data.Result[0].Stream["app"] != "x" {
		t.Errorf("unexpected result: %+v", res.Data.Result)
	}
}

func TestBearerAuthAndOrg(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK, `{"status":"success","data":{"result":[]}}`, cap)
	defer srv.Close()

	cfg := lokiCfg(srv.URL)
	cfg.OrgID = "tenant-a"
	cfg.Auth.Token = "tok123"
	cfg.Auth.Username = "ignored" // token wins
	c := New(cfg)

	if _, err := c.QueryRange(context.Background(), `{a="b"}`, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if cap.authHeader != "Bearer tok123" {
		t.Errorf("auth header = %q", cap.authHeader)
	}
	if cap.basicOK {
		t.Errorf("basic auth should not be set when token present")
	}
	if cap.orgID != "tenant-a" {
		t.Errorf("org = %q", cap.orgID)
	}
}

func TestBasicAuth(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK, `{"status":"success","data":{"result":[]}}`, cap)
	defer srv.Close()

	cfg := lokiCfg(srv.URL)
	cfg.Auth.Username = "user"
	cfg.Auth.Password = "pass"
	c := New(cfg)

	if _, err := c.QueryRange(context.Background(), `{a="b"}`, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if !cap.basicOK || cap.basicUser != "user" || cap.basicPass != "pass" {
		t.Errorf("basic auth = %q/%q ok=%v", cap.basicUser, cap.basicPass, cap.basicOK)
	}
}

func TestLabelNamesScoping(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK, `{"status":"success","data":["app","ns"]}`, cap)
	defer srv.Close()

	c := New(lokiCfg(srv.URL))

	// With a scope query.
	res, err := c.LabelNames(context.Background(), 10, 20, `{ns="a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if cap.path != "/loki/api/v1/labels" {
		t.Errorf("path = %q", cap.path)
	}
	if cap.query != `{ns="a"}` {
		t.Errorf("scope query = %q", cap.query)
	}
	if len(res.Data) != 2 {
		t.Errorf("data = %v", res.Data)
	}

	// Without a scope query, the query param must be omitted entirely.
	c.LabelNames(context.Background(), 10, 20, "")
	if _, ok := cap.queryValues["query"]; ok {
		t.Errorf("query param should be omitted when scope is empty, got %v", cap.queryValues["query"])
	}
}

func TestLabelValuesPathEscaped(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK, `{"status":"success","data":["prod","dev"]}`, cap)
	defer srv.Close()

	c := New(lokiCfg(srv.URL))
	if _, err := c.LabelValues(context.Background(), "env", 10, 20, `{ns="a"}`); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/loki/api/v1/label/env/values" {
		t.Errorf("path = %q", cap.path)
	}
	if cap.query != `{ns="a"}` {
		t.Errorf("scope query = %q", cap.query)
	}
}

func TestBaseURLWithPrefix(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK, `{"status":"success","data":{"result":[]}}`, cap)
	defer srv.Close()

	// Base URL already includes the API prefix — must not be doubled.
	c := New(lokiCfg(srv.URL + "/loki/api/v1"))
	if _, err := c.QueryRange(context.Background(), `{a="b"}`, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if cap.path != "/loki/api/v1/query_range" {
		t.Errorf("path = %q", cap.path)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusBadRequest, "boom", cap)
	defer srv.Close()

	c := New(lokiCfg(srv.URL))
	_, err := c.QueryRange(context.Background(), `{a="b"}`, 1, 2, 3)
	if err == nil {
		t.Fatal("expected error on HTTP 400")
	}
}

func TestLokiStatusError(t *testing.T) {
	cap := &capture{}
	srv := newFakeLoki(t, http.StatusOK, `{"status":"error","error":"bad query"}`, cap)
	defer srv.Close()

	c := New(lokiCfg(srv.URL))
	_, err := c.QueryRange(context.Background(), `{a="b"}`, 1, 2, 3)
	if err == nil {
		t.Fatal("expected error on status:error response")
	}
}
