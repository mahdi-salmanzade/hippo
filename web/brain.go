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
