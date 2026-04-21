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
// - callers can run the library with zero YAML of their own.
package yaml

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mahdi-salmanzade/hippo"

	yamlv3 "gopkg.in/yaml.v3"
)

// pollInterval is how often the watch goroutine stats the policy
// file. It is a var (not const) so tests can compress it to
// milliseconds. Production code must not mutate it.
var pollInterval = 2 * time.Second

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
// goroutine. Defaults to a discard logger - routing itself is not
// chatty.
func WithLogger(l *slog.Logger) Option {
	return func(r *router) { r.logger = l }
}

// router implements hippo.Router. The *Policy pointer is swapped
// atomically so Route is lock-free, and the hot-reload goroutine (if
// any) is stopped via the stop channel. *router is also an io.Closer
// - callers who enabled WithWatch should type-assert and Close to
// stop the watcher goroutine on shutdown.
type router struct {
	path     string
	policy   atomic.Pointer[Policy]
	watch    bool
	logger   *slog.Logger
	stop     chan struct{}
	stopOnce sync.Once
	// lastMtime is the last-observed mtime of the policy file;
	// guarded by the watcher goroutine (only one writer).
	lastMtime time.Time
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
	if r.watch && path != "" {
		if stat, err := os.Stat(path); err == nil {
			r.lastMtime = stat.ModTime()
		}
		go r.watchLoop()
	}
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

// Close stops the hot-reload watcher goroutine (no-op if watch was
// not enabled). Idempotent and safe to call from any goroutine.
// hippo.Router does not require Close; type-assert the Load() return
// to io.Closer when you want to release the watcher.
func (r *router) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	return nil
}

// watchLoop runs in its own goroutine when WithWatch(true) is set.
// It polls the policy file's mtime and atomically swaps the Policy
// pointer on change. Parse failures during reload are logged at Warn
// and the previous policy remains authoritative - a broken edit
// should not take down a running Brain.
func (r *router) watchLoop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.checkAndReload()
		}
	}
}

func (r *router) checkAndReload() {
	stat, err := os.Stat(r.path)
	if err != nil {
		r.logger.Warn("router/yaml: stat failed during reload; keeping current policy",
			"path", r.path, "err", err)
		return
	}
	if !stat.ModTime().After(r.lastMtime) {
		return // unchanged
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.logger.Warn("router/yaml: read failed during reload; keeping current policy",
			"path", r.path, "err", err)
		return
	}
	newPolicy, err := parsePolicy(data)
	if err != nil {
		r.logger.Warn("router/yaml: parse failed during reload; keeping current policy",
			"path", r.path, "err", err)
		return
	}
	r.policy.Store(newPolicy)
	r.lastMtime = stat.ModTime()
	r.logger.Info("router/yaml: policy reloaded", "path", r.path)
}

// Route picks a Provider+Model for c.
//
// Algorithm:
//  1. Look up TaskPolicy for c.Task. If absent, return ErrUnknownTask.
//  2. Compute effective privacy = max(Call.Privacy, TaskPolicy.Privacy).
//  3. Compute effective cost ceiling = min of non-zero
//     (TaskPolicy.MaxCostUSD, Call.MaxCostUSD, budget).
//  4. Walk Prefer, then Fallback. For each "provider:model" slug,
//     skip if the provider isn't registered, privacy tier is too
//     weak, or estimated cost exceeds the ceiling.
//  5. Return the first viable Decision with Reason tagged "preferred"
//     or "fallback". If both lists exhaust, return
//     ErrNoRoutableProvider.
//
// Route is lock-free: the policy pointer is loaded once atomically
// and reused for the rest of the call, so a concurrent hot-reload
// can swap in a new policy without affecting calls already in flight.
func (r *router) Route(ctx context.Context, c hippo.Call, providers []hippo.Provider, budget float64) (hippo.Decision, error) {
	_ = ctx // reserved for cancellation checks when Route grows more work

	// Explicit model pin wins over policy. A caller that set Call.Model
	// (e.g. the web UI's Model dropdown) wants *that* model, not
	// whatever the task rule would select. We pick the first registered
	// provider that can price it; if none can, we fall through to
	// normal policy routing so the caller still gets a usable decision.
	if c.Model != "" {
		for _, p := range providers {
			if _, err := p.EstimateCost(c); err != nil {
				continue
			}
			cost, _ := p.EstimateCost(c)
			return hippo.Decision{
				Provider:         p.Name(),
				Model:            c.Model,
				EstimatedCostUSD: cost,
				Reason:           "pinned model: " + c.Model,
			}, nil
		}
	}

	pol := r.policy.Load()
	if pol == nil {
		return hippo.Decision{}, hippo.ErrNoRoutableProvider
	}
	tp, ok := pol.Tasks[c.Task]
	if !ok {
		return hippo.Decision{}, fmt.Errorf("%w: %q", hippo.ErrUnknownTask, c.Task)
	}

	privacy := c.Privacy
	if tp.Privacy > privacy {
		privacy = tp.Privacy
	}
	costCap := minNonZero(tp.MaxCostUSD, c.MaxCostUSD, budget)

	if d, ok := r.tryList(c, tp.Prefer, providers, privacy, costCap, "preferred"); ok {
		return d, nil
	}
	if d, ok := r.tryList(c, tp.Fallback, providers, privacy, costCap, "fallback"); ok {
		return d, nil
	}
	return hippo.Decision{}, hippo.ErrNoRoutableProvider
}

// tryList walks a single preference/fallback list and returns the
// first viable Decision. Skips the candidate silently for each
// disqualifier so the caller can fall through to the next list.
func (r *router) tryList(c hippo.Call, slugs []string, providers []hippo.Provider, minPrivacy hippo.PrivacyTier, costCap float64, reasonTag string) (hippo.Decision, bool) {
	for _, slug := range slugs {
		providerName, model, ok := splitSlug(slug)
		if !ok {
			r.logger.Warn("router/yaml: malformed slug; skipping", "slug", slug)
			continue
		}
		p := providerByName(providers, providerName)
		if p == nil {
			continue // provider not registered
		}
		if p.Privacy() < minPrivacy {
			continue // provider can't meet the privacy requirement
		}
		callCopy := c
		callCopy.Model = model
		cost, err := p.EstimateCost(callCopy)
		if err != nil {
			// Unknown model within a registered provider is a
			// silent skip - the router tries the next candidate.
			r.logger.Debug("router/yaml: EstimateCost failed; skipping candidate",
				"slug", slug, "err", err)
			continue
		}
		if cost > costCap {
			continue
		}
		return hippo.Decision{
			Provider:         providerName,
			Model:            model,
			EstimatedCostUSD: cost,
			Reason:           reasonTag + " match: " + slug,
		}, true
	}
	return hippo.Decision{}, false
}

// providerByName returns the registered provider with the given
// Name() or nil if not present. Linear scan is fine for the small
// provider sets hippo supports.
func providerByName(providers []hippo.Provider, name string) hippo.Provider {
	for _, p := range providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// splitSlug splits "provider:model" - empty on either side is
// rejected. Model strings may themselves contain colons (e.g. the
// dated "claude-haiku-4-5-20250930" form), so we split on the first
// colon only.
func splitSlug(s string) (provider, model string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// minNonZero returns the minimum of the arguments, ignoring values
// that are zero (treated as "no cap"). If all arguments are zero,
// returns +Inf - i.e. unlimited.
func minNonZero(vals ...float64) float64 {
	m := math.Inf(1)
	for _, v := range vals {
		if v <= 0 {
			continue
		}
		if v < m {
			m = v
		}
	}
	return m
}

// discardWriter is used as the fallback slog sink; it exists in this
// package to avoid pulling in io.Discard and a second import.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
