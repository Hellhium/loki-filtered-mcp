package enforcer

import (
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"
)

// TestCorpusEverySelectorEnforced runs a broad corpus of valid LogQL through the
// enforcer and asserts the invariant that matters for security: every stream
// selector in the enforced output carries the enforced matcher, and the output
// re-parses as valid LogQL.
func TestCorpusEverySelectorEnforced(t *testing.T) {
	enf := New([]LabelFilter{{Matcher: eq("namespace", "team-a"), Allowed: []string{"team-a"}}}, ModeOverride)

	corpus := []string{
		`{app="foo"}`,
		`{app="foo"} |= "error"`,
		`{app="foo"} |= "error" != "debug" | json | line_format "{{.msg}}"`,
		`{app="foo"} | logfmt | duration > 10s`,
		`rate({app="foo"}[5m])`,
		`sum(rate({app="foo"}[5m]))`,
		`sum by (level) (rate({app="foo"}[5m]))`,
		`sum without (pod) (count_over_time({app="foo"}[1m]))`,
		`topk(10, sum by (app) (rate({app="foo"}[5m])))`,
		`count_over_time({app="foo"}[1m]) > 5`,
		`(sum(rate({app="a"}[1m])) / sum(rate({app="b"}[1m]))) * 100`,
		`sum_over_time({app="foo"} | unwrap bytes [5m])`,
		`quantile_over_time(0.99, {app="foo"} | unwrap latency [5m]) by (path)`,
		`avg_over_time({app="foo"} | json | unwrap value [10m])`,
		`bytes_rate({app="foo"}[5m])`,
		`{app="foo"} |~ "5.." | pattern "<_> status=<status>"`,
		`label_replace(rate({app="foo"}[5m]), "svc", "$1", "app", "(.*)")`,
		`{app="foo", env="prod"} | line_format "{{ .app }}"`,
		`max by (app) (max_over_time({app="foo"} | unwrap n [5m]))`,
		`count_over_time({app="a"}[1m]) + count_over_time({app="b"}[1m]) + count_over_time({app="c"}[1m])`,
	}

	for _, q := range corpus {
		t.Run(q, func(t *testing.T) {
			out, err := enf.Enforce(q)
			if err != nil {
				t.Fatalf("Enforce(%q) failed: %v", q, err)
			}
			expr, err := syntax.ParseExpr(out)
			if err != nil {
				t.Fatalf("enforced output %q does not re-parse: %v", out, err)
			}
			sites := 0
			expr.Walk(func(node syntax.Expr) bool {
				me, ok := node.(*syntax.MatchersExpr)
				if !ok {
					return true
				}
				sites++
				found := false
				for _, m := range me.Mts {
					if m.Name == "namespace" && m.Type == labels.MatchEqual && m.Value == "team-a" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("selector %q in %q lacks the enforced matcher", me.String(), out)
				}
				return true
			})
			if sites == 0 {
				t.Errorf("no selectors found in enforced output %q", out)
			}
		})
	}
}
