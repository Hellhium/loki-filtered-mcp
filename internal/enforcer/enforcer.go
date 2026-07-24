// Package enforcer injects a fixed set of label matchers into every stream
// selector of a LogQL query. It is the security core of loki-filtered-mcp:
// enforcement happens on the parsed LogQL AST (never by string concatenation),
// so a caller cannot widen the query's scope past the configured filters.
//
// Narrowing within scope is allowed. A caller may further constrain an enforced
// label as long as it stays inside the configured allowed values — e.g. with
// namespace restricted to {team-a, team-b}, a query for namespace="team-a"
// returns only team-a. Because the enforced matcher is always AND-ed into the
// selector, any caller matcher whose intersection with the allowed set is
// non-empty is safe: the result can never reach outside the allowed values.
//
// The collision policy governs only the case where a caller's matcher on an
// enforced label excludes every allowed value (so the two cannot both hold):
//
//   - ModeReject:   refuse the whole query with a typed error.
//   - ModeOverride: drop the caller's matcher(s) on that label and fall back to
//     the enforced matcher.
package enforcer

import (
	"errors"
	"fmt"
	"strings"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"
)

// Mode selects the behaviour when a query already constrains an enforced label.
type Mode int

const (
	// ModeReject refuses a query whose matcher on an enforced label conflicts
	// with the allowed values (excludes all of them). This is the fail-closed
	// default. Narrowing within the allowed set is always permitted.
	ModeReject Mode = iota
	// ModeOverride drops a caller's conflicting matcher on an enforced label and
	// falls back to the enforced one. Narrowing within scope is still preserved.
	ModeOverride
)

func (m Mode) String() string {
	switch m {
	case ModeOverride:
		return "override"
	case ModeReject:
		return "reject"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

var (
	// ErrParse is returned when the input query is not valid LogQL. The wrapped
	// parser error is safe to surface to the caller.
	ErrParse = errors.New("failed to parse LogQL query")

	// ErrConflict is returned in ModeReject when a matcher on an enforced label
	// excludes every allowed value, so it can never be satisfied within scope.
	ErrConflict = errors.New("query conflicts with an enforced label matcher")

	// ErrNoSelector is returned when a query contains no stream selector for the
	// enforced matchers to attach to (e.g. a literal-only expression). We fail
	// closed rather than let an unfiltered expression through.
	ErrNoSelector = errors.New("query contains no stream selector to enforce")
)

// LabelFilter is one enforced constraint handed to the enforcer: the compiled
// matcher to inject plus the set of literal values it permits. The allowed set
// is what lets the enforcer tell a legitimate narrowing (namespace="team-a")
// apart from a conflict (namespace="evil").
type LabelFilter struct {
	Matcher *labels.Matcher
	Allowed []string
}

// Enforcer injects a fixed, ordered set of matchers into LogQL queries.
type Enforcer struct {
	// filters is the ordered set of enforced constraints, at most one per label
	// name. Order is preserved in the output for deterministic serialization.
	filters []LabelFilter
	// byName indexes filters by label name for O(1) lookups.
	byName map[string]LabelFilter
	mode   Mode
}

// New builds an Enforcer from the (already validated) enforced filters, at most
// one per label name.
func New(filters []LabelFilter, mode Mode) *Enforcer {
	byName := make(map[string]LabelFilter, len(filters))
	for _, f := range filters {
		byName[f.Matcher.Name] = f
	}
	return &Enforcer{filters: filters, byName: byName, mode: mode}
}

// Selector returns the enforced matchers serialized as a LogQL stream selector,
// e.g. `{namespace=~"team-a|team-b", env="prod"}`. It is used to scope Loki's
// label APIs. Returns "{}" if there are no enforced matchers.
func (e *Enforcer) Selector() string {
	mts := make([]*labels.Matcher, len(e.filters))
	for i, f := range e.filters {
		mts[i] = f.Matcher
	}
	return (&syntax.MatchersExpr{Mts: mts}).String()
}

// Enforce parses q, injects the enforced matchers into every stream selector,
// and returns the re-serialized query. On any error the returned string is
// empty and no partially-enforced query is ever emitted.
func (e *Enforcer) Enforce(q string) (string, error) {
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrParse, err)
	}

	var (
		sites   int
		walkErr error
	)
	expr.Walk(func(node syntax.Expr) bool {
		if walkErr != nil {
			return false
		}
		me, ok := node.(*syntax.MatchersExpr)
		if !ok {
			return true
		}
		sites++
		mts, err := e.enforceMatchers(me.Mts)
		if err != nil {
			walkErr = err
			return false
		}
		me.Mts = mts
		return true
	})
	if walkErr != nil {
		return "", walkErr
	}

	// Fail closed: an expression that reaches Loki must carry at least one
	// stream selector we could constrain. A literal-only query (e.g. "1 + 1")
	// touches no log data, but rejecting it is the safe choice for a security
	// boundary.
	if sites == 0 {
		return "", fmt.Errorf("%w: %q", ErrNoSelector, q)
	}

	return expr.String(), nil
}

// enforceMatchers returns the matcher list for a single selector with the
// enforced filters applied.
//
// Matchers on non-enforced labels are kept in their original order. Matchers on
// each enforced label are grouped and evaluated against that label's allowed
// value set:
//
//   - Compatible (some allowed value satisfies all of the caller's matchers on
//     the label): the caller's matchers are kept — this is a narrowing within
//     scope. The enforced matcher is also appended to guarantee the bound,
//     unless the caller already pinned the label to a single allowed value with
//     an equality matcher (which makes the enforced matcher redundant).
//   - Conflict (no allowed value can satisfy the caller's matchers): ModeReject
//     returns an error; ModeOverride drops the caller's matchers and uses the
//     enforced matcher instead.
//
// The enforced matcher is always AND-ed in for compatible cases, so the result
// can never match a stream outside the allowed set.
func (e *Enforcer) enforceMatchers(targets []*labels.Matcher) ([]*labels.Matcher, error) {
	res := make([]*labels.Matcher, 0, len(targets)+len(e.filters))
	grouped := make(map[string][]*labels.Matcher)

	for _, target := range targets {
		if _, ok := e.byName[target.Name]; ok {
			grouped[target.Name] = append(grouped[target.Name], target)
		} else {
			// Not an enforced label — keep it untouched, in place.
			res = append(res, target)
		}
	}

	for _, f := range e.filters {
		callers := grouped[f.Matcher.Name]
		if len(callers) == 0 {
			// Caller said nothing about this label — inject the enforced matcher.
			res = append(res, f.Matcher)
			continue
		}

		if !anyAllowedSatisfies(f.Allowed, callers) {
			// The caller's constraint excludes every allowed value.
			if e.mode == ModeReject {
				return nil, fmt.Errorf("%w: %s on %q excludes every allowed value %v",
					ErrConflict, joinMatchers(callers), f.Matcher.Name, f.Allowed)
			}
			// ModeOverride: drop the caller's matchers, fall back to enforced.
			res = append(res, f.Matcher)
			continue
		}

		// Compatible narrowing — keep the caller's matchers.
		pinned := false
		for _, c := range callers {
			res = append(res, c)
			if c.Type == labels.MatchEqual && contains(f.Allowed, c.Value) {
				// Pins the label to a single allowed value, so the enforced
				// matcher would be redundant.
				pinned = true
			}
		}
		if !pinned {
			res = append(res, f.Matcher)
		}
	}

	return res, nil
}

// anyAllowedSatisfies reports whether at least one allowed value is matched by
// every one of the caller's matchers (i.e. the intersection with the allowed
// set is non-empty).
func anyAllowedSatisfies(allowed []string, callers []*labels.Matcher) bool {
	for _, v := range allowed {
		ok := true
		for _, c := range callers {
			if !c.Matches(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func joinMatchers(ms []*labels.Matcher) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.String()
	}
	return strings.Join(parts, ", ")
}
