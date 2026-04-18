// Package yaml provides a YAML-policy-driven hippo.Router
// implementation.
//
// A Policy is a per-TaskKind map of preferences ("prefer") and
// fall-backs ("fallback"), each a list of "provider:model" slugs.
// Route walks prefer then fallback, skipping any candidate whose
// privacy tier, budget constraint, or task-level cost ceiling would
// block the Call, and returns the first viable Decision.
//
// Load("") returns a Router backed by the embedded default_policy.yaml
// — callers can run the library with zero YAML of their own.
package yaml

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/mahdi-salmanzade/hippo"

	yamlv3 "gopkg.in/yaml.v3"
)

//go:embed default_policy.yaml
var defaultPolicyYAML []byte

// Policy is the declarative routing config. yaml.Unmarshal maps a
// YAML document into this shape; hot-reload swaps this pointer
// atomically when the source file changes.
type Policy struct {
	// Tasks maps each TaskKind to its per-task policy.
	Tasks map[hippo.TaskKind]TaskPolicy `yaml:"tasks"`
}

// TaskPolicy is the per-TaskKind rule set.
type TaskPolicy struct {
	// Privacy is the minimum tier required. Defaults to PrivacyCloudOK.
	Privacy hippo.PrivacyTier
	// Prefer lists "provider:model" slugs in preference order.
	Prefer []string
	// Fallback lists "provider:model" slugs tried only after Prefer
	// exhausts.
	Fallback []string
	// MaxCostUSD caps the per-call estimate for this task. Zero
	// means "no task-level cap"; Call.MaxCostUSD and the budget's
	// Remaining() still apply.
	MaxCostUSD float64
}

// UnmarshalYAML lets TaskPolicy read the privacy tier as a human
// token ("cloud_ok" / "sensitive_redact" / "local_only") instead of
// the underlying int. Unknown tokens surface as a parse error so
// typos don't silently downgrade to the zero value.
func (tp *TaskPolicy) UnmarshalYAML(node *yamlv3.Node) error {
	var raw struct {
		Privacy    string   `yaml:"privacy"`
		Prefer     []string `yaml:"prefer"`
		Fallback   []string `yaml:"fallback"`
		MaxCostUSD float64  `yaml:"max_cost_usd"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(raw.Privacy)) {
	case "", "cloud_ok":
		tp.Privacy = hippo.PrivacyCloudOK
	case "sensitive_redact":
		tp.Privacy = hippo.PrivacySensitiveRedact
	case "local_only":
		tp.Privacy = hippo.PrivacyLocalOnly
	default:
		return fmt.Errorf("router/yaml: unknown privacy tier %q", raw.Privacy)
	}
	tp.Prefer = raw.Prefer
	tp.Fallback = raw.Fallback
	tp.MaxCostUSD = raw.MaxCostUSD
	return nil
}

// Option configures a router during construction.
type Option func(*router)

// WithWatch enables hot-reload: the router polls its source file's
// mtime and swaps the in-memory Policy atomically on change. Parse
// failures during reload are logged but do not replace the active
// policy. Disabled by default.
func WithWatch(enabled bool) Option {
	return func(r *router) { r.watch = enabled }
}

// WithLogger sets the structured logger used by the hot-reload
// goroutine. Defaults to a discard logger — routing itself is not
// chatty.
func WithLogger(l *slog.Logger) Option {
	return func(r *router) { r.logger = l }
}

// router implements hippo.Router. The *Policy pointer is swapped
// atomically so Route is lock-free.
type router struct {
	path   string
	policy atomic.Pointer[Policy]
	watch  bool
	logger *slog.Logger
	stop   chan struct{}
}

// Load parses a YAML policy file from path. When path is empty the
// embedded default_policy.yaml is used, which is enough to route
// Anthropic's three Claude 4.x models.
func Load(path string, opts ...Option) (hippo.Router, error) {
	var data []byte
	if path == "" {
		data = defaultPolicyYAML
	} else {
		f, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("router/yaml: read %s: %w", path, err)
		}
		data = f
	}
	r, err := newRouter(data, opts...)
	if err != nil {
		return nil, err
	}
	r.path = path
	// TODO(pass3.5): spin up the hot-reload watcher when r.watch is
	// true. Split into the next commit to keep diffs focused.
	return r, nil
}

// LoadBytes parses an in-memory YAML document. Useful for tests and
// for callers that assemble policies programmatically.
func LoadBytes(data []byte, opts ...Option) (hippo.Router, error) {
	return newRouter(data, opts...)
}

// newRouter builds a *router without touching the filesystem. Shared
// by Load and LoadBytes.
func newRouter(data []byte, opts ...Option) (*router, error) {
	p, err := parsePolicy(data)
	if err != nil {
		return nil, err
	}
	r := &router{
		logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		stop:   make(chan struct{}),
	}
	for _, o := range opts {
		o(r)
	}
	r.policy.Store(p)
	return r, nil
}

func parsePolicy(data []byte) (*Policy, error) {
	var p Policy
	if err := yamlv3.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("router/yaml: parse: %w", err)
	}
	return &p, nil
}

// Name returns "yaml".
func (r *router) Name() string { return "yaml" }

// Route is a stub until Pass 3.4. Returning ErrNotImplemented keeps
// the package compilable and lets the next commit focus on the
// selection logic without touching the loader.
func (r *router) Route(ctx context.Context, c hippo.Call, providers []hippo.Provider, budget float64) (hippo.Decision, error) {
	_ = ctx
	_ = c
	_ = providers
	_ = budget
	return hippo.Decision{}, hippo.ErrNotImplemented
}

// discardWriter is used as the fallback slog sink; it exists in this
// package to avoid pulling in io.Discard and a second import.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
