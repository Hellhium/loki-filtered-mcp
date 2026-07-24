package enforcer

import (
	"errors"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"
)

func eq(name, value string) *labels.Matcher {
	return labels.MustNewMatcher(labels.MatchEqual, name, value)
}

func re(name, value string) *labels.Matcher {
	return labels.MustNewMatcher(labels.MatchRegexp, name, value)
}

// nsEnforcer enforces namespace="team-a" (single value → equality matcher).
func nsEnforcer(mode Mode) *Enforcer {
	return New([]LabelFilter{{Matcher: eq("namespace", "team-a"), Allowed: []string{"team-a"}}}, mode)
}

// nsMultiEnforcer enforces namespace=~"team-a|team-b" (multi value → regex).
func nsMultiEnforcer(mode Mode) *Enforcer {
	return New([]LabelFilter{{Matcher: re("namespace", "team-a|team-b"), Allowed: []string{"team-a", "team-b"}}}, mode)
}

func TestEnforce(t *testing.T) {
	tests := []struct {
		name    string
		enf     *Enforcer
		query   string
		want    string
		wantErr error
	}{
		// --- basic injection into a variety of selector positions ---
		{
			name:  "bare selector",
			enf:   nsEnforcer(ModeReject),
			query: `{app="foo"}`,
			want:  `{app="foo", namespace="team-a"}`,
		},
		{
			name:  "line filter pipeline",
			enf:   nsEnforcer(ModeReject),
			query: `{app="foo"} |= "boom" | json`,
			want:  `{app="foo", namespace="team-a"} |= "boom" | json`,
		},
		{
			name:  "rate inside sum by",
			enf:   nsEnforcer(ModeReject),
			query: `sum by(level) (rate({app="foo"}[5m]))`,
			want:  `sum by (level)(rate({app="foo", namespace="team-a"}[5m]))`,
		},
		{
			name:  "count_over_time with binary op",
			enf:   nsEnforcer(ModeReject),
			query: `count_over_time({app="x"}[1m]) > 5`,
			want:  `(count_over_time({app="x", namespace="team-a"}[1m]) > 5)`,
		},
		{
			name:  "unwrap expression",
			enf:   nsEnforcer(ModeReject),
			query: `sum(sum_over_time({app="x"} | unwrap bytes [5m]))`,
			want:  `sum(sum_over_time({app="x", namespace="team-a"} | unwrap bytes[5m]))`,
		},
		{
			name:  "empty selector body gets a matcher",
			enf:   nsEnforcer(ModeReject),
			query: `{app="foo"}`,
			want:  `{app="foo", namespace="team-a"}`,
		},

		// --- multi-value (regex) enforced matcher ---
		{
			name:  "multi-value regex injected",
			enf:   nsMultiEnforcer(ModeReject),
			query: `{app="foo"}`,
			want:  `{app="foo", namespace=~"team-a|team-b"}`,
		},

		// --- redundant restatement of the exact enforced matcher is allowed ---
		{
			name:  "exact restatement is deduped, not duplicated",
			enf:   nsEnforcer(ModeReject),
			query: `{namespace="team-a", app="foo"}`,
			want:  `{app="foo", namespace="team-a"}`,
		},
		{
			name:  "exact restatement, override mode",
			enf:   nsEnforcer(ModeOverride),
			query: `{namespace="team-a"}`,
			want:  `{namespace="team-a"}`,
		},

		// --- narrowing within scope is allowed (the key feature) ---
		{
			name:  "narrow multi-value set to one allowed value",
			enf:   nsMultiEnforcer(ModeReject),
			query: `{app="foo", namespace="team-a"}`,
			want:  `{app="foo", namespace="team-a"}`,
		},
		{
			name:  "narrow to allowed value, override mode too",
			enf:   nsMultiEnforcer(ModeOverride),
			query: `{namespace="team-b"}`,
			want:  `{namespace="team-b"}`,
		},
		{
			name:  "narrow with a subset regex of the allowed set",
			enf:   nsMultiEnforcer(ModeReject),
			query: `{namespace=~"team-a"}`,
			want:  `{namespace=~"team-a", namespace=~"team-a|team-b"}`,
		},
		{
			name:  "exclude one of several allowed values (keeps the rest)",
			enf:   nsMultiEnforcer(ModeReject),
			query: `{app="x", namespace!="team-a"}`,
			want:  `{app="x", namespace!="team-a", namespace=~"team-a|team-b"}`,
		},
		{
			name:  "narrowing inside an aggregation",
			enf:   nsMultiEnforcer(ModeReject),
			query: `sum(rate({app="x", namespace="team-a"}[5m]))`,
			want:  `sum(rate({app="x", namespace="team-a"}[5m]))`,
		},

		// --- adversarial: attempts to widen or escape scope, reject mode ---
		{
			name:    "restate enforced label with different value → reject",
			enf:     nsEnforcer(ModeReject),
			query:   `{namespace="evil"}`,
			wantErr: ErrConflict,
		},
		{
			name:    "negated enforced label → reject",
			enf:     nsEnforcer(ModeReject),
			query:   `{app="foo", namespace!="team-a"}`,
			wantErr: ErrConflict,
		},
		{
			name:    "regex disjoint from allowed values → reject",
			enf:     nsEnforcer(ModeReject),
			query:   `{namespace=~"prod-.+"}`,
			wantErr: ErrConflict,
		},
		{
			name:    "conflict deep inside aggregation → reject",
			enf:     nsEnforcer(ModeReject),
			query:   `sum(rate({namespace="evil"}[5m]))`,
			wantErr: ErrConflict,
		},

		// --- adversarial: same attempts, override mode rewrites them ---
		{
			name:  "restate enforced label → overridden",
			enf:   nsEnforcer(ModeOverride),
			query: `{namespace="evil"}`,
			want:  `{namespace="team-a"}`,
		},
		{
			name:  "negated enforced label → overridden",
			enf:   nsEnforcer(ModeOverride),
			query: `{app="foo", namespace!="team-a"}`,
			want:  `{app="foo", namespace="team-a"}`,
		},
		{
			name:  "regex disjoint from allowed values → overridden",
			enf:   nsEnforcer(ModeOverride),
			query: `{namespace=~"prod-.+"}`,
			want:  `{namespace="team-a"}`,
		},
		{
			name:  "override keeps other matchers, replaces enforced",
			enf:   nsMultiEnforcer(ModeOverride),
			query: `{app="foo", namespace="evil", env="prod"}`,
			want:  `{app="foo", env="prod", namespace=~"team-a|team-b"}`,
		},

		// --- every selector in a multi-selector expression is enforced ---
		{
			name:  "binary op over two selectors, both enforced",
			enf:   nsEnforcer(ModeReject),
			query: `count_over_time({app="a"}[1m]) / count_over_time({app="b"}[1m])`,
			want:  `(count_over_time({app="a", namespace="team-a"}[1m]) / count_over_time({app="b", namespace="team-a"}[1m]))`,
		},
		{
			name:    "binary op, conflict in one leg → reject whole query",
			enf:     nsEnforcer(ModeReject),
			query:   `count_over_time({app="a"}[1m]) / count_over_time({namespace="evil"}[1m])`,
			wantErr: ErrConflict,
		},

		// --- parser errors surface as ErrParse ---
		{
			name:    "syntactically invalid query",
			enf:     nsEnforcer(ModeReject),
			query:   `{app=}`,
			wantErr: ErrParse,
		},
		{
			name:    "not a stream selector at all",
			enf:     nsEnforcer(ModeReject),
			query:   `hello world`,
			wantErr: ErrParse,
		},

		// --- fail closed: literal-only expression has no selector ---
		{
			name:    "literal-only expression → no selector",
			enf:     nsEnforcer(ModeReject),
			query:   `1 + 1`,
			wantErr: ErrNoSelector,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.enf.Enforce(tc.query)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Enforce(%q) error = %v, want %v", tc.query, err, tc.wantErr)
				}
				if got != "" {
					t.Fatalf("Enforce(%q) returned %q on error; want empty string", tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enforce(%q) unexpected error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Fatalf("Enforce(%q)\n  got:  %s\n  want: %s", tc.query, got, tc.want)
			}
			// The enforced output must itself be valid LogQL.
			if _, err := syntax.ParseExpr(got); err != nil {
				t.Fatalf("enforced output %q does not re-parse: %v", got, err)
			}
		})
	}
}

// TestEnforceValuesWithRegexMeta makes sure enforced values containing regex
// metacharacters are matched literally (config is expected to QuoteMeta them
// before handing us a regex matcher, but multi-value matchers still round-trip).
func TestEnforceRegexMetaValues(t *testing.T) {
	// Values "a.b" and "c+d" quoted → "a\.b|c\+d".
	enf := New([]LabelFilter{{Matcher: re("namespace", `a\.b|c\+d`), Allowed: []string{"a.b", "c+d"}}}, ModeReject)
	got, err := enf.Enforce(`{app="x"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{app="x", namespace=~"a\\.b|c\\+d"}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// TestEnforceMultipleLabels enforces two labels at once and asserts both land
// in every selector, in configured order.
func TestEnforceMultipleLabels(t *testing.T) {
	enf := New([]LabelFilter{
		{Matcher: eq("namespace", "team-a"), Allowed: []string{"team-a"}},
		{Matcher: eq("env", "prod"), Allowed: []string{"prod"}},
	}, ModeReject)
	got, err := enf.Enforce(`{app="x"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{app="x", namespace="team-a", env="prod"}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
