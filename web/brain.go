package web

import (
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/ollama"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
	yamlrouter "github.com/mahdi-salmanzade/hippo/router/yaml"
)

// BrainBundle groups everything the web server constructed for a given
// Config so the server can swap it atomically after a config edit and
// close the previous bundle's owned resources on shutdown.
type BrainBundle struct {
	Brain    *hippo.Brain
	Memory   hippo.Memory
	Budget   hippo.BudgetTracker
	Router   hippo.Router
	Warnings []string
}

// Close releases resources owned by the bundle. Safe to call on a nil
// bundle.
func (b *BrainBundle) Close() error {
	if b == nil {
		return nil
	}
	var errs []error
	if b.Memory != nil {
		if err := b.Memory.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if closer, ok := b.Router.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.Brain != nil {
		_ = b.Brain.Close() // memory already closed; ignore
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("web: bundle close: %v", errs)
}

// BuildBrain constructs a BrainBundle from cfg. Providers missing
// credentials or disabled are skipped silently; skipped reasons surface
// in bundle.Warnings so the UI can show them.
//
// A Config with zero enabled providers still builds successfully, with
// Brain == nil. The web UI keeps serving the config page in that state
// so the user can add credentials.
func BuildBrain(cfg *Config, logger *slog.Logger) (*BrainBundle, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	bundle := &BrainBundle{}
	var opts []hippo.Option
	opts = append(opts, hippo.WithLogger(logger))

	// Providers. Order (alphabetical by name) determines the no-router
	// fallback pick; the router overrides this once attached.
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	providerCount := 0
	for _, name := range names {
		pc := cfg.Providers[name]
		if !pc.Enabled {
			continue
		}
		p, warn, err := constructProvider(name, pc)
		if err != nil {
			bundle.Warnings = append(bundle.Warnings, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if warn != "" {
			bundle.Warnings = append(bundle.Warnings, warn)
		}
		if p != nil {
			opts = append(opts, hippo.WithProvider(p))
			providerCount++
		}
	}
	if providerCount == 0 {
		return bundle, nil
	}

	// Memory.
	if cfg.Memory.Enabled {
		dbPath, err := ExpandPath(cfg.Memory.DBPath)
		if err != nil {
			return nil, fmt.Errorf("web: memory db path: %w", err)
		}
		if dbPath == "" {
			dbPath = ":memory:"
		}
		mem, err := sqlite.Open(dbPath)
		if err != nil {
			return nil, fmt.Errorf("web: open memory: %w", err)
		}
		bundle.Memory = mem
		opts = append(opts, hippo.WithMemory(mem))
	}

	// Budget.
	var budOpts []budget.Option
	if cfg.Budget.CeilingUSD > 0 {
		budOpts = append(budOpts, budget.WithCeiling(cfg.Budget.CeilingUSD))
	}
	bud := budget.New(budOpts...)
	bundle.Budget = bud
	opts = append(opts, hippo.WithBudget(bud))

	// Router.
	policyPath, err := ExpandPath(cfg.PolicyPath)
	if err != nil {
		return nil, fmt.Errorf("web: policy path: %w", err)
	}
	router, err := yamlrouter.Load(policyPath, yamlrouter.WithLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("web: load policy: %w", err)
	}
	bundle.Router = router
	opts = append(opts, hippo.WithRouter(router))

	brain, err := hippo.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("web: build brain: %w", err)
	}
	bundle.Brain = brain
	return bundle, nil
}

// constructProvider builds one hippo.Provider from a (name, pc) pair.
// Returns (nil, warn, nil) when the provider is skippable (empty key,
// unreachable daemon); returns (nil, "", err) when construction itself
// failed in a way the user should see.
func constructProvider(name string, pc ProviderConfig) (hippo.Provider, string, error) {
	switch name {
	case "anthropic":
		if pc.APIKey == "" {
			return nil, "anthropic: API key empty; skipping", nil
		}
		opts := []anthropic.Option{anthropic.WithAPIKey(pc.APIKey)}
		if pc.DefaultModel != "" {
			opts = append(opts, anthropic.WithModel(pc.DefaultModel))
		}
		p, err := anthropic.New(opts...)
		return p, "", err
	case "openai":
		if pc.APIKey == "" {
			return nil, "openai: API key empty; skipping", nil
		}
		opts := []openai.Option{openai.WithAPIKey(pc.APIKey)}
		if pc.DefaultModel != "" {
			opts = append(opts, openai.WithModel(pc.DefaultModel))
		}
		if pc.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(pc.BaseURL))
		}
		p, err := openai.New(opts...)
		return p, "", err
	case "ollama":
		opts := []ollama.Option{}
		if pc.BaseURL != "" {
			opts = append(opts, ollama.WithBaseURL(pc.BaseURL))
		}
		if pc.DefaultModel != "" {
			opts = append(opts, ollama.WithModel(pc.DefaultModel))
		}
		p, err := ollama.New(opts...)
		return p, "", err
	default:
		return nil, "", fmt.Errorf("unknown provider %q", name)
	}
}
