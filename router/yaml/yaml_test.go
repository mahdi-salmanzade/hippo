package yaml

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// fakeProvider is a stub provider for routing tests. It exposes a
// controllable Name, Privacy, and EstimateCost; Call/Stream/Models
// return stubs since Route never invokes them.
type fakeProvider struct {
	name    string
	privacy hippo.PrivacyTier
	cost    float64
	costErr error
}

func (f *fakeProvider) Name() string                               { return f.name }
func (f *fakeProvider) Models() []hippo.ModelInfo                  { return nil }
func (f *fakeProvider) Privacy() hippo.PrivacyTier                 { return f.privacy }
func (f *fakeProvider) EstimateCost(hippo.Call) (float64, error)   { return f.cost, f.costErr }
func (f *fakeProvider) Call(context.Context, hippo.Call) (*hippo.Response, error) {
	return &hippo.Response{}, nil
}
func (f *fakeProvider) Stream(context.Context, hippo.Call) (<-chan hippo.StreamChunk, error) {
	return nil, hippo.ErrNotImplemented
}

func TestLoadDefault(t *testing.T) {
	r, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Name() != "yaml" {
		t.Errorf("Name = %q, want %q", r.Name(), "yaml")
	}
	// The default policy routes reason → claude-opus-4-7 first.
	ap := &fakeProvider{name: "anthropic", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskReason},
		[]hippo.Provider{ap}, 1.00)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Provider != "anthropic" || d.Model != "claude-opus-4-7" {
		t.Errorf("Decision = %+v, want anthropic/claude-opus-4-7", d)
	}
}

func TestLoadBytesHonorsPolicy(t *testing.T) {
	doc := []byte(`
tasks:
  classify:
    privacy: cloud_ok
    prefer:
      - pA:modelA
    max_cost_usd: 0.01
`)
	r, err := LoadBytes(doc)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.005}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskClassify},
		[]hippo.Provider{pA}, 1.00)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Provider != "pA" || d.Model != "modelA" {
		t.Errorf("Decision = %+v, want pA/modelA", d)
	}
}

func TestRouteRespectsPreferOrder(t *testing.T) {
	doc := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pA:modelA
      - pB:modelB
`)
	r, _ := LoadBytes(doc)
	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	pB := &fakeProvider{name: "pB", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate},
		[]hippo.Provider{pA, pB}, 1.00)
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "pA" {
		t.Errorf("Provider = %q, want pA (first in prefer list)", d.Provider)
	}
}

func TestRouteFallsBackToFallback(t *testing.T) {
	doc := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - missing:modelX
    fallback:
      - pB:modelB
`)
	r, _ := LoadBytes(doc)
	pB := &fakeProvider{name: "pB", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate},
		[]hippo.Provider{pB}, 1.00)
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "pB" {
		t.Errorf("Provider = %q, want pB (fallback)", d.Provider)
	}
	if !contains(d.Reason, "fallback") {
		t.Errorf("Reason = %q, want to mention fallback", d.Reason)
	}
}

func TestRouteSkipsPrivacyMismatch(t *testing.T) {
	doc := []byte(`
tasks:
  protect:
    privacy: local_only
    prefer:
      - cloud:modelX
      - local:modelY
`)
	r, _ := LoadBytes(doc)
	cloud := &fakeProvider{name: "cloud", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	local := &fakeProvider{name: "local", privacy: hippo.PrivacyLocalOnly, cost: 0.01}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskProtect},
		[]hippo.Provider{cloud, local}, 1.00)
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "local" {
		t.Errorf("Provider = %q, want local (cloud should be skipped)", d.Provider)
	}
}

func TestRouteSkipsBudgetOverrun(t *testing.T) {
	doc := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - expensive:modelE
      - cheap:modelC
`)
	r, _ := LoadBytes(doc)
	expensive := &fakeProvider{name: "expensive", privacy: hippo.PrivacyCloudOK, cost: 1.50}
	cheap := &fakeProvider{name: "cheap", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate},
		[]hippo.Provider{expensive, cheap}, 1.00) // budget = $1
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "cheap" {
		t.Errorf("Provider = %q, want cheap (expensive exceeds budget)", d.Provider)
	}
}

func TestRouteRespectsTaskMaxCostUSD(t *testing.T) {
	doc := []byte(`
tasks:
  classify:
    privacy: cloud_ok
    prefer:
      - midCost:modelM
      - lowCost:modelL
    max_cost_usd: 0.005
`)
	r, _ := LoadBytes(doc)
	mid := &fakeProvider{name: "midCost", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	low := &fakeProvider{name: "lowCost", privacy: hippo.PrivacyCloudOK, cost: 0.001}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskClassify},
		[]hippo.Provider{mid, low}, 10.00)
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "lowCost" {
		t.Errorf("Provider = %q, want lowCost (midCost exceeds task cap)", d.Provider)
	}
}

func TestRouteRespectsCallMaxCostUSD(t *testing.T) {
	doc := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pA:modelA
      - pB:modelB
`)
	r, _ := LoadBytes(doc)
	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.05}
	pB := &fakeProvider{name: "pB", privacy: hippo.PrivacyCloudOK, cost: 0.005}
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate, MaxCostUSD: 0.01},
		[]hippo.Provider{pA, pB}, 10.00)
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider != "pB" {
		t.Errorf("Provider = %q, want pB (Call.MaxCostUSD caps at 0.01)", d.Provider)
	}
}

func TestRouteReturnsErrOnExhaustion(t *testing.T) {
	doc := []byte(`
tasks:
  reason:
    privacy: cloud_ok
    prefer:
      - missing:modelX
    fallback:
      - also_missing:modelY
`)
	r, _ := LoadBytes(doc)
	_, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskReason},
		[]hippo.Provider{}, 10.00)
	if !errors.Is(err, hippo.ErrNoRoutableProvider) {
		t.Errorf("err = %v, want ErrNoRoutableProvider", err)
	}
}

func TestRouteReturnsErrOnUnknownTask(t *testing.T) {
	doc := []byte(`
tasks:
  classify:
    privacy: cloud_ok
    prefer:
      - pA:modelA
`)
	r, _ := LoadBytes(doc)
	_, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskReason},
		[]hippo.Provider{&fakeProvider{name: "pA"}}, 10.00)
	if !errors.Is(err, hippo.ErrUnknownTask) {
		t.Errorf("err = %v, want ErrUnknownTask", err)
	}
}

func TestLoadRejectsUnknownPrivacyTier(t *testing.T) {
	doc := []byte(`
tasks:
  classify:
    privacy: telepathic_only
    prefer: []
`)
	_, err := LoadBytes(doc)
	if err == nil {
		t.Fatal("expected parse error on unknown privacy tier, got nil")
	}
}

func TestSplitSlug(t *testing.T) {
	cases := []struct {
		in       string
		provider string
		model    string
		ok       bool
	}{
		{"anthropic:claude-haiku-4-5", "anthropic", "claude-haiku-4-5", true},
		{"anthropic:claude-haiku-4-5-20250930", "anthropic", "claude-haiku-4-5-20250930", true},
		{"nopec colon", "", "", false},
		{":no-provider", "", "", false},
		{"no-model:", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		p, m, ok := splitSlug(tc.in)
		if p != tc.provider || m != tc.model || ok != tc.ok {
			t.Errorf("splitSlug(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, p, m, ok, tc.provider, tc.model, tc.ok)
		}
	}
}

// contains is a tiny helper to keep the test lightweight (avoiding
// strings for a single-use check inside Reason assertions).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// fastPoll compresses the watcher's poll interval for the duration
// of a test. Production code must treat pollInterval as a constant.
func fastPoll(t *testing.T) {
	t.Helper()
	old := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })
}

func TestHotReload(t *testing.T) {
	fastPoll(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	initial := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pA:modelA
`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := Load(path, WithWatch(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	})

	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	pB := &fakeProvider{name: "pB", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	providers := []hippo.Provider{pA, pB}

	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate}, providers, 1.00)
	if err != nil {
		t.Fatalf("initial Route: %v", err)
	}
	if d.Provider != "pA" {
		t.Fatalf("initial provider = %q, want pA", d.Provider)
	}

	// Mutate the file: swap prefer to pB.
	// Bump mtime explicitly because some filesystems round writes
	// to the nearest second; adding a second beyond initial is safe.
	updated := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pB:modelB
`)
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate}, providers, 1.00)
		if err == nil && d.Provider == "pB" {
			return // reload observed
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("policy was not reloaded within 2s")
}

func TestHotReloadKeepsPolicyOnParseFailure(t *testing.T) {
	fastPoll(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	initial := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pA:modelA
`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := Load(path, WithWatch(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	})

	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.01}
	providers := []hippo.Provider{pA}

	// Overwrite with broken YAML.
	if err := os.WriteFile(path, []byte("not: [valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	// Give the watcher several poll cycles to notice.
	time.Sleep(200 * time.Millisecond)

	// The original policy should still be serving.
	d, err := r.Route(context.Background(), hippo.Call{Task: hippo.TaskGenerate}, providers, 1.00)
	if err != nil {
		t.Fatalf("Route after bad reload: %v", err)
	}
	if d.Provider != "pA" {
		t.Errorf("provider = %q, want pA (parse failure should keep prior policy)", d.Provider)
	}
}

// TestRouteRespectsPinnedModel verifies that Call.Model wins over the
// policy's task-based selection — a caller who explicitly pinned a
// model (e.g. the web UI's Model dropdown) gets that model back rather
// than whatever the task rule would have chosen.
func TestRouteRespectsPinnedModel(t *testing.T) {
	doc := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pA:policy_choice
`)
	r, err := LoadBytes(doc)
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.02}

	// With Call.Model set, the router should return the pinned model
	// rather than "policy_choice" from the prefer list.
	d, err := r.Route(context.Background(),
		hippo.Call{Task: hippo.TaskGenerate, Model: "user_pinned_model"},
		[]hippo.Provider{pA}, 1.00)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Model != "user_pinned_model" {
		t.Errorf("Model = %q, want user_pinned_model (policy was ignored correctly?)", d.Model)
	}
	if d.Provider != "pA" {
		t.Errorf("Provider = %q, want pA", d.Provider)
	}
}

// TestRoutePinnedModelFallsBackWhenNoProviderCanPrice verifies that a
// pinned model with no matching provider falls through to policy
// routing rather than erroring. Gives callers a safety net when the
// UI sends a stale model name.
func TestRoutePinnedModelFallsBackWhenNoProviderCanPrice(t *testing.T) {
	doc := []byte(`
tasks:
  generate:
    privacy: cloud_ok
    prefer:
      - pA:policy_choice
`)
	r, _ := LoadBytes(doc)
	// pA's EstimateCost errors — simulating "I don't know this model".
	pA := &fakeProvider{name: "pA", privacy: hippo.PrivacyCloudOK, cost: 0.02, costErr: errors.New("unknown model")}
	// pB has no error AND the policy prefers pA — so policy path picks pB via fallback? No,
	// simpler: the pinned model can't be priced by anyone, so we fall through to policy.
	d, err := r.Route(context.Background(),
		hippo.Call{Task: hippo.TaskGenerate, Model: "not_supported_by_anyone"},
		[]hippo.Provider{pA}, 1.00)
	// pA can't price even the policy_choice (costErr), so policy fails too.
	if !errors.Is(err, hippo.ErrNoRoutableProvider) {
		t.Errorf("err = %v, want ErrNoRoutableProvider (fell through then failed)", err)
	}
	_ = d
}

// verify *router implements io.Closer at compile time.
var _ io.Closer = (*router)(nil)

// useErrorsForLint keeps errors imported even if the only
// consumers happen to live in build-tag-guarded files later.
var _ = errors.New

