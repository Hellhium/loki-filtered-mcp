// Package config loads and validates the single YAML file that drives
// loki-filtered-mcp. It is the source of truth for the listen address, the
// upstream Loki endpoint(s), and the set of *instances* — each instance being a
// bearer-token-authenticated scope with its own filters, enforcement policy and
// choice of exposed surfaces (MCP and/or the Loki proxy).
//
// The YAML-facing types use pointer fields so "absent" is distinguishable from
// "set to the zero value": top-level loki/enforcement/defaults blocks supply
// values that every instance inherits, and an instance overrides only the leaf
// keys it sets. Filters are the one exception — they are never inherited or
// merged, because an implicit change to a filter set is exactly the kind of
// silent scope change this project exists to prevent.
//
// Load resolves that inheritance once at startup into []ResolvedInstance, whose
// fields carry no optionals. Everything downstream receives a fully-resolved
// value, so "did this inherit?" can never be asked at request time.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"gopkg.in/yaml.v3"

	"github.com/Hellhium/loki-filtered-mcp/internal/enforcer"
)

// Duration is a time.Duration that unmarshals from a YAML string like "30s".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string ("30s", "1h", "500ms").
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Secret is a credential read from the config — an instance bearer token or an
// upstream Loki password/token. Every rendering method redacts it, so an
// accidental %v of a config struct, a log line, or a YAML dump cannot leak a
// credential. Call Reveal at the exact point the plaintext is needed.
type Secret string

const redacted = "[redacted]"

// Reveal returns the plaintext. This is the only way to read a Secret.
func (s Secret) Reveal() string { return string(s) }

// String renders the secret as "[redacted]" (or "" when unset).
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

// GoString redacts under %#v.
func (s Secret) GoString() string { return `"` + s.String() + `"` }

// MarshalYAML redacts when a config struct is re-serialized.
func (s Secret) MarshalYAML() (any, error) { return s.String(), nil }

// MarshalJSON redacts when a config struct is serialized to JSON.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }

// --- YAML-facing types ------------------------------------------------------

// Config is the top-level parsed YAML document. Loki, Enforcement and Defaults
// hold the values inherited by every instance.
type Config struct {
	Server      Server              `yaml:"server"`
	Loki        LokiOverride        `yaml:"loki"`
	Enforcement EnforcementOverride `yaml:"enforcement"`
	Defaults    DefaultsOverride    `yaml:"defaults"`
	Instances   []Instance          `yaml:"instances"`

	// resolved is filled by Load; see Resolved.
	resolved []ResolvedInstance
}

// Server holds the HTTP listener settings shared by every instance.
type Server struct {
	Listen string `yaml:"listen"`
}

// LokiOverride describes the upstream Loki endpoint. Every field is optional:
// unset keys inherit from the level above (built-in default → top level →
// instance).
type LokiOverride struct {
	URL     *string       `yaml:"url"`
	OrgID   *string       `yaml:"org_id"`
	Auth    *AuthOverride `yaml:"auth"`
	Timeout *Duration     `yaml:"timeout"`
}

// AuthOverride carries the credentials sent to Loki (not the credentials
// clients present to us — those are InstanceAuth).
type AuthOverride struct {
	Username *string `yaml:"username"`
	Password *Secret `yaml:"password"`
	Token    *Secret `yaml:"token"`
}

// EnforcementOverride controls collision handling and label-API scoping.
type EnforcementOverride struct {
	OnConflict       *string `yaml:"on_conflict"`
	EnforceLabelAPIs *bool   `yaml:"enforce_label_apis"`
}

// DefaultsOverride supplies fallback query parameters.
type DefaultsOverride struct {
	Limit *int      `yaml:"limit"`
	Since *Duration `yaml:"since"`
}

// Instance is one authenticated scope. Its bearer token is what selects it: a
// request carrying that token is served this instance's filters, enforcement
// policy and endpoints, and nothing else.
type Instance struct {
	Name      string       `yaml:"name"`
	Auth      InstanceAuth `yaml:"auth"`
	Endpoints Endpoints    `yaml:"endpoints"`
	Filters   []Filter     `yaml:"filters"`

	// Overrides of the top-level blocks. nil means "inherit everything".
	Enforcement *EnforcementOverride `yaml:"enforcement"`
	Loki        *LokiOverride        `yaml:"loki"`
	Defaults    *DefaultsOverride    `yaml:"defaults"`
}

// InstanceAuth is how clients authenticate to this instance. Type is a closed
// set so an unrecognized value can never be read as "no authentication".
type InstanceAuth struct {
	Type   string   `yaml:"type"`
	Tokens []Secret `yaml:"tokens"`
}

// AuthTypeBearer is the only implemented client authentication method.
const AuthTypeBearer = "bearer"

// Endpoints selects the surfaces an instance exposes. The fields are pointers
// so an explicit "false" is distinguishable from an omitted key.
type Endpoints struct {
	MCP   *bool `yaml:"mcp"`
	Proxy *bool `yaml:"proxy"`
}

// Filter is one enforced label constraint. A single value produces an equality
// matcher (label="v"); multiple values produce a regex matcher (label=~"v1|v2").
type Filter struct {
	Label  string   `yaml:"label"`
	Values []string `yaml:"values"`
}

// --- resolved types ---------------------------------------------------------

// Loki is a fully-resolved upstream endpoint.
type Loki struct {
	URL     string
	OrgID   string
	Auth    Auth
	Timeout Duration
}

// Auth is a fully-resolved set of upstream credentials. A bearer token takes
// precedence over basic auth.
type Auth struct {
	Username string
	Password Secret
	Token    Secret
}

// Enforcement is a fully-resolved enforcement policy.
type Enforcement struct {
	OnConflict       string
	EnforceLabelAPIs bool
}

// Defaults is a fully-resolved set of query defaults.
type Defaults struct {
	Limit int
	Since Duration
}

// ResolvedInstance is one instance with all inheritance applied. It is what
// every other package consumes.
type ResolvedInstance struct {
	Name        string
	Tokens      []Secret
	MCP         bool
	Proxy       bool
	Filters     []Filter
	Enforcement Enforcement
	Loki        Loki
	Defaults    Defaults
}

// Built-in defaults, applied before the top-level YAML blocks.
const (
	defaultListen           = ":8080"
	defaultTimeout          = 30 * time.Second
	defaultLimit            = 100
	defaultSince            = time.Hour
	defaultConflict         = "reject"
	defaultEnforceLabelAPIs = true

	// The MCP server is this project's primary purpose; the Loki-compatible
	// proxy is opt-in per instance.
	defaultEndpointMCP   = true
	defaultEndpointProxy = false
)

// Load reads, strictly decodes, resolves and validates the YAML config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject unknown keys — typos should not be silently ignored
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Server.Listen == "" {
		cfg.Server.Listen = defaultListen
	}
	cfg.resolved = cfg.Resolve()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Resolved returns the instances with inheritance applied, in config order.
func (c *Config) Resolved() []ResolvedInstance { return c.resolved }

// Resolve folds the built-in defaults, the top-level blocks and each instance's
// overrides into fully-resolved instances. It performs no validation.
func (c *Config) Resolve() []ResolvedInstance {
	globalLoki := applyLoki(Loki{Timeout: Duration(defaultTimeout)}, &c.Loki)
	globalEnf := applyEnforcement(Enforcement{
		OnConflict:       defaultConflict,
		EnforceLabelAPIs: defaultEnforceLabelAPIs,
	}, &c.Enforcement)
	globalDef := applyDefaults(Defaults{
		Limit: defaultLimit,
		Since: Duration(defaultSince),
	}, &c.Defaults)

	out := make([]ResolvedInstance, 0, len(c.Instances))
	for _, in := range c.Instances {
		out = append(out, ResolvedInstance{
			Name:   strings.TrimSpace(in.Name),
			Tokens: append([]Secret(nil), in.Auth.Tokens...),
			MCP:    boolOr(in.Endpoints.MCP, defaultEndpointMCP),
			Proxy:  boolOr(in.Endpoints.Proxy, defaultEndpointProxy),
			// Filters are copied verbatim — never inherited, never merged.
			Filters:     append([]Filter(nil), in.Filters...),
			Enforcement: applyEnforcement(globalEnf, in.Enforcement),
			Loki:        applyLoki(globalLoki, in.Loki),
			Defaults:    applyDefaults(globalDef, in.Defaults),
		})
	}
	return out
}

func applyLoki(base Loki, o *LokiOverride) Loki {
	if o == nil {
		return base
	}
	if o.URL != nil {
		base.URL = *o.URL
	}
	if o.OrgID != nil {
		base.OrgID = *o.OrgID
	}
	if o.Timeout != nil {
		base.Timeout = *o.Timeout
	}
	if o.Auth != nil {
		if o.Auth.Username != nil {
			base.Auth.Username = *o.Auth.Username
		}
		if o.Auth.Password != nil {
			base.Auth.Password = *o.Auth.Password
		}
		if o.Auth.Token != nil {
			base.Auth.Token = *o.Auth.Token
		}
	}
	return base
}

func applyEnforcement(base Enforcement, o *EnforcementOverride) Enforcement {
	if o == nil {
		return base
	}
	if o.OnConflict != nil {
		base.OnConflict = strings.TrimSpace(*o.OnConflict)
	}
	if o.EnforceLabelAPIs != nil {
		base.EnforceLabelAPIs = *o.EnforceLabelAPIs
	}
	return base
}

func applyDefaults(base Defaults, o *DefaultsOverride) Defaults {
	if o == nil {
		return base
	}
	if o.Limit != nil {
		base.Limit = *o.Limit
	}
	if o.Since != nil {
		base.Since = *o.Since
	}
	return base
}

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

// --- validation -------------------------------------------------------------

// validate checks the raw instance declarations and their resolved forms. Every
// failure is fatal: a config that is ambiguous about scope must not start.
func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("at least one instance is required")
	}

	seenName := make(map[string]bool, len(c.Instances))
	seenToken := make(map[string]string, len(c.Instances)) // token → instance name

	for i, raw := range c.Instances {
		ri := c.resolved[i]
		where := fmt.Sprintf("instances[%d]", i)
		if ri.Name != "" {
			where = fmt.Sprintf("instances[%d] (%s)", i, ri.Name)
		}

		if ri.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if seenName[ri.Name] {
			return fmt.Errorf("%s: duplicate instance name %q", where, ri.Name)
		}
		seenName[ri.Name] = true

		// Client authentication.
		if raw.Auth.Type != AuthTypeBearer {
			return fmt.Errorf("%s: auth.type must be %q, got %q", where, AuthTypeBearer, raw.Auth.Type)
		}
		if len(ri.Tokens) == 0 {
			return fmt.Errorf("%s: at least one auth token is required", where)
		}
		// Token strength is the operator's call — config.example.yaml advises a
		// length, but a short token is not rejected here. Only tokens that
		// cannot work at all are refused.
		for j, tok := range ri.Tokens {
			plain := tok.Reveal()
			if plain == "" {
				return fmt.Errorf("%s: auth.tokens[%d] must not be empty", where, j)
			}
			if !isHeaderSafe(plain) {
				return fmt.Errorf("%s: auth.tokens[%d] contains characters that are not valid in an Authorization header", where, j)
			}
			if other, dup := seenToken[plain]; dup {
				return fmt.Errorf("%s: auth token is also used by instance %q — a token must grant exactly one scope", where, other)
			}
			seenToken[plain] = ri.Name
		}

		// Exposed surfaces.
		if !ri.MCP && !ri.Proxy {
			return fmt.Errorf("%s: endpoints.mcp and endpoints.proxy are both false — the instance would be unreachable", where)
		}

		if err := ri.validate(where); err != nil {
			return err
		}
	}
	return nil
}

// validate checks a single resolved instance: upstream, policy, and filters.
func (ri ResolvedInstance) validate(where string) error {
	// Loki URL must be an absolute http(s) URL.
	if ri.Loki.URL == "" {
		return fmt.Errorf("%s: loki.url is required (set it at the top level or on the instance)", where)
	}
	u, err := url.Parse(ri.Loki.URL)
	if err != nil {
		return fmt.Errorf("%s: loki.url is not a valid URL: %w", where, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: loki.url scheme must be http or https, got %q", where, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: loki.url must include a host", where)
	}
	if ri.Loki.Timeout <= 0 {
		return fmt.Errorf("%s: loki.timeout must be positive", where)
	}

	// Conflict policy.
	switch ri.Enforcement.OnConflict {
	case "reject", "override":
	default:
		return fmt.Errorf("%s: enforcement.on_conflict must be \"reject\" or \"override\", got %q",
			where, ri.Enforcement.OnConflict)
	}

	// Query defaults.
	if ri.Defaults.Limit <= 0 {
		return fmt.Errorf("%s: defaults.limit must be positive", where)
	}
	if ri.Defaults.Since <= 0 {
		return fmt.Errorf("%s: defaults.since must be positive", where)
	}

	// At least one filter, each well-formed. An instance with no filters would
	// be an unrestricted Loki proxy, defeating the purpose.
	if len(ri.Filters) == 0 {
		return fmt.Errorf("%s: at least one filter is required (filters are never inherited)", where)
	}
	seen := make(map[string]bool, len(ri.Filters))
	for i, f := range ri.Filters {
		if strings.TrimSpace(f.Label) == "" {
			return fmt.Errorf("%s: filters[%d]: label must not be empty", where, i)
		}
		// Use legacy validation ([a-zA-Z_][a-zA-Z0-9_]*): that is the label-name
		// grammar Loki's LogQL stream selectors accept. (The default UTF-8
		// scheme in newer prometheus/common would accept almost anything.)
		if !model.LabelName(f.Label).IsValidLegacy() {
			return fmt.Errorf("%s: filters[%d]: %q is not a valid label name", where, i, f.Label)
		}
		if seen[f.Label] {
			return fmt.Errorf("%s: filters[%d]: duplicate filter for label %q", where, i, f.Label)
		}
		seen[f.Label] = true
		if len(f.Values) == 0 {
			return fmt.Errorf("%s: filters[%d] (%s): at least one value is required", where, i, f.Label)
		}
		for j, v := range f.Values {
			if v == "" {
				return fmt.Errorf("%s: filters[%d] (%s): values[%d] must not be empty", where, i, f.Label, j)
			}
		}
	}

	// The matchers must compile and must not be empty-compatible (a matcher that
	// accepts "" could widen scope to unlabeled streams).
	ms, err := ri.Matchers()
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	for _, m := range ms {
		if m.Matches("") {
			return fmt.Errorf("%s: filter on %q would match the empty string, which is not allowed", where, m.Name)
		}
	}
	return nil
}

// isHeaderSafe reports whether s consists only of printable, non-space ASCII —
// the characters that can appear in an Authorization header value unescaped.
func isHeaderSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// --- accessors used by the enforcer and handlers ----------------------------

// FilterValues returns the configured allowed values for an enforced label and
// true if the label is enforced; otherwise nil and false.
func (ri ResolvedInstance) FilterValues(label string) ([]string, bool) {
	for _, f := range ri.Filters {
		if f.Label == label {
			return f.Values, true
		}
	}
	return nil, false
}

// Mode returns the enforcer collision mode for this instance.
func (ri ResolvedInstance) Mode() enforcer.Mode {
	if ri.Enforcement.OnConflict == "override" {
		return enforcer.ModeOverride
	}
	return enforcer.ModeReject
}

// EnforcedFilters compiles the instance filters into enforcer inputs: each
// carries the compiled matcher plus the allowed literal values, so the enforcer
// can distinguish a legitimate narrowing from a conflict.
func (ri ResolvedInstance) EnforcedFilters() ([]enforcer.LabelFilter, error) {
	ms, err := ri.Matchers()
	if err != nil {
		return nil, err
	}
	out := make([]enforcer.LabelFilter, len(ms))
	for i := range ms {
		out[i] = enforcer.LabelFilter{Matcher: ms[i], Allowed: ri.Filters[i].Values}
	}
	return out, nil
}

// Matchers compiles the filter set into enforced label matchers, one per filter,
// in configured order. One value → equality matcher; many → regex alternation
// with each value regexp-quoted so metacharacters are matched literally.
func (ri ResolvedInstance) Matchers() ([]*labels.Matcher, error) {
	ms := make([]*labels.Matcher, 0, len(ri.Filters))
	for _, f := range ri.Filters {
		var (
			m   *labels.Matcher
			err error
		)
		if len(f.Values) == 1 {
			m, err = labels.NewMatcher(labels.MatchEqual, f.Label, f.Values[0])
		} else {
			quoted := make([]string, len(f.Values))
			for i, v := range f.Values {
				quoted[i] = regexp.QuoteMeta(v)
			}
			m, err = labels.NewMatcher(labels.MatchRegexp, f.Label, strings.Join(quoted, "|"))
		}
		if err != nil {
			return nil, fmt.Errorf("filter on %q: %w", f.Label, err)
		}
		ms = append(ms, m)
	}
	return ms, nil
}
