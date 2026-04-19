package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/mcp"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/ollama"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
	yamlrouter "github.com/mahdi-salmanzade/hippo/router/yaml"
)

// Compile-time assertions that the sqlite store implements the
// capability interfaces. They keep the type-assertion sites honest
// even if the sqlite package's method signatures drift.
var (
	_ backfillStarter  = (*sqliteBackfillStub)(nil)
	_ autoPruneStarter = (*sqliteBackfillStub)(nil)
)

// sqliteBackfillStub is never instantiated; it exists only so the
// interface assertions above compile-check against the real store.
type sqliteBackfillStub struct{}

func (*sqliteBackfillStub) StartBackfill(ctx context.Context, cfg sqlite.BackfillConfig) (func(), error) {
	return nil, nil
}
func (*sqliteBackfillStub) StartAutoPrune(ctx context.Context, cfg sqlite.PruneConfig, interval time.Duration) (func(), error) {
	return nil, nil
}

// BrainBundle groups everything the web server constructed for a given
// Config so the server can swap it atomically after a config edit and
// close the previous bundle's owned resources on shutdown.
type BrainBundle struct {
	Brain      *hippo.Brain
	Memory     hippo.Memory
	Budget     hippo.BudgetTracker
	Router     hippo.Router
	MCPClients []*mcp.Client
	Warnings   []string
}

// Close releases resources owned by the bundle. Safe to call on a nil
// bundle.
func (b *BrainBundle) Close() error {
	if b == nil {
		return nil
	}
	var errs []error
	for _, c := range b.MCPClients {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
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

	// Memory + embedder + backfill + auto-prune. Wiring order:
	//   1. build the embedder (if configured) so the store can carry
	//      its name for Model tracking at row level.
	//   2. Open the store with sqlite.WithEmbedder so Recall can
	//      reach the embedder.
	//   3. After Open, start the backfill worker (lifetime tracked by
	//      the store's worker list so Close unwinds it).
	//   4. Start the auto-prune worker with the user-configured or
	//      default policy.
	if cfg.Memory.Enabled {
		dbPath, err := ExpandPath(cfg.Memory.DBPath)
		if err != nil {
			return nil, fmt.Errorf("web: memory db path: %w", err)
		}
		if dbPath == "" {
			dbPath = ":memory:"
		}

		var embedder hippo.Embedder
		if cfg.Memory.Embedder.Provider == "ollama" ||
			(cfg.Memory.Embedder.Provider == "" && cfg.Providers["ollama"].Enabled) {
			embedder = buildEmbedder(cfg)
		}

		var memOpts []sqlite.Option
		if embedder != nil {
			memOpts = append(memOpts, sqlite.WithEmbedder(embedder))
			opts = append(opts, hippo.WithEmbedder(embedder))
		}
		mem, err := sqlite.Open(dbPath, memOpts...)
		if err != nil {
			return nil, fmt.Errorf("web: open memory: %w", err)
		}
		bundle.Memory = mem
		opts = append(opts, hippo.WithMemory(mem))

		// The concrete store supports StartBackfill / StartAutoPrune;
		// type-assert against narrow interfaces to avoid leaking the
		// package-private type.
		if embedder != nil {
			if bf, ok := mem.(backfillStarter); ok {
				bfCfg := sqlite.BackfillConfig{Embedder: embedder}
				if n := cfg.Memory.Embedder.BackfillBatchSize; n > 0 {
					bfCfg.BatchSize = n
				}
				if sec := cfg.Memory.Embedder.BackfillIntervalS; sec > 0 {
					bfCfg.Interval = time.Duration(sec) * time.Second
				}
				if _, err := bf.StartBackfill(context.Background(), bfCfg); err != nil {
					logger.Warn("memory: StartBackfill failed", "err", err)
				}
			}
		}
		if ap, ok := mem.(autoPruneStarter); ok {
			pruneCfg, interval := pruneFromConfig(cfg.Memory.Prune)
			if _, err := ap.StartAutoPrune(context.Background(), pruneCfg, interval); err != nil {
				logger.Warn("memory: StartAutoPrune failed", "err", err)
			}
		}
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

	// MCP clients. Each enabled server gets 10s to complete the
	// initialize handshake; servers that exceed the budget, fail to
	// spawn, or reject the handshake are logged+skipped so one dead
	// stdio binary can't block hippo serve.
	mcpClients, mcpWarns := connectMCPServers(cfg.MCP.Servers, logger)
	bundle.MCPClients = mcpClients
	bundle.Warnings = append(bundle.Warnings, mcpWarns...)
	if len(mcpClients) > 0 {
		sources := make([]hippo.MCPToolSource, 0, len(mcpClients))
		for _, c := range mcpClients {
			sources = append(sources, c)
		}
		opts = append(opts, hippo.WithMCPClients(sources...))
	}

	brain, err := hippo.New(opts...)
	if err != nil {
		// Tear down any MCP clients we opened — hippo.New's own
		// collision check is the typical failure mode, and leaving
		// subprocesses running would leak across bundle rebuilds.
		for _, c := range mcpClients {
			_ = c.Close()
		}
		bundle.MCPClients = nil
		return nil, fmt.Errorf("web: build brain: %w", err)
	}
	bundle.Brain = brain
	return bundle, nil
}

// mcpConnectTimeout bounds the per-server handshake at bundle
// construction time. Servers that take longer are skipped with a
// warning; the Client's own reconnect loop continues in the
// background and will recover once they become reachable.
const mcpConnectTimeout = 10 * time.Second

// connectMCPServers launches one mcp.Client per enabled entry,
// returning the live clients plus a warning string for every
// skipped or failed server.
func connectMCPServers(servers []MCPServerConfig, logger *slog.Logger) ([]*mcp.Client, []string) {
	var clients []*mcp.Client
	var warns []string
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		prefix := s.Prefix
		if prefix == "" {
			prefix = s.Name
		}
		ctx, cancel := context.WithTimeout(context.Background(), mcpConnectTimeout)
		var (
			c   *mcp.Client
			err error
		)
		switch s.Transport {
		case "stdio":
			c, err = mcp.Connect(ctx, s.Command,
				mcp.WithPrefix(prefix),
				mcp.WithLogger(logger),
			)
		case "http":
			c, err = mcp.ConnectHTTPWithHeaders(ctx, s.URL, headersFromMap(s.Headers),
				mcp.WithPrefix(prefix),
				mcp.WithLogger(logger),
			)
		default:
			err = fmt.Errorf("unsupported transport %q", s.Transport)
		}
		cancel()
		if err != nil {
			msg := fmt.Sprintf("mcp %q: %v", s.Name, err)
			logger.Warn("mcp: connect failed; skipping", "server", s.Name, "err", err)
			warns = append(warns, msg)
			continue
		}
		clients = append(clients, c)
	}
	return clients, warns
}

// headersFromMap converts the config's plain map into http.Header.
// A nil map yields an empty header (not nil) so downstream code can
// always Clone without a nil check.
func headersFromMap(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

// backfillStarter is the optional capability a hippo.Memory may
// expose to run an embedding backfill worker. Matched by the concrete
// sqlite store; other memory backends can implement it when they
// grow embedding support.
type backfillStarter interface {
	StartBackfill(ctx context.Context, cfg sqlite.BackfillConfig) (func(), error)
}

// autoPruneStarter likewise gates auto-prune wiring behind a narrow
// interface so the web package stays backend-agnostic.
type autoPruneStarter interface {
	StartAutoPrune(ctx context.Context, cfg sqlite.PruneConfig, interval time.Duration) (func(), error)
}

// buildEmbedder constructs the embedder described by cfg.Memory.Embedder.
// Only the ollama backend is supported today; the Provider field is a
// future-proofing hook.
func buildEmbedder(cfg *Config) hippo.Embedder {
	ec := cfg.Memory.Embedder
	model := ec.Model
	if model == "" {
		model = ollama.DefaultEmbeddingModel
	}
	baseURL := ec.BaseURL
	if baseURL == "" {
		baseURL = cfg.Providers["ollama"].BaseURL
	}
	opts := []ollama.EmbedderOption{ollama.WithEmbedderModel(model)}
	if baseURL != "" {
		opts = append(opts, ollama.WithEmbedderBaseURL(baseURL))
	}
	return ollama.NewEmbedder(opts...)
}

// pruneFromConfig maps the YAML-friendly PruneConfigBlock onto the
// concrete sqlite.PruneConfig, falling back to DefaultPruneConfig for
// any zero values.
func pruneFromConfig(p PruneConfigBlock) (sqlite.PruneConfig, time.Duration) {
	out := sqlite.DefaultPruneConfig()
	if p.WorkingMaxAgeHours > 0 {
		out.WorkingMaxAge = time.Duration(p.WorkingMaxAgeHours) * time.Hour
	}
	if p.EpisodicMaxAgeHours > 0 {
		out.EpisodicMaxAge = time.Duration(p.EpisodicMaxAgeHours) * time.Hour
	}
	if p.EpisodicImportanceCutoff > 0 {
		out.EpisodicImportanceCutoff = p.EpisodicImportanceCutoff
	}
	interval := time.Hour
	if p.AutoPruneIntervalMinutes > 0 {
		interval = time.Duration(p.AutoPruneIntervalMinutes) * time.Minute
	}
	return out, interval
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
