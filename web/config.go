// Package web implements hippo's embedded HTTP UI: a single-binary web
// frontend covering configuration, spend, policy, and a chat playground.
//
// All templates and static assets are compiled into the binary via
// go:embed so the server has zero runtime filesystem dependencies beyond
// the user's config and memory database.
package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk shape of ~/.hippo/config.yaml. It is parsed on
// startup and written back atomically whenever the user edits provider
// credentials or policy from the web UI.
//
// Comment preservation: Save strips inline comments and regenerates a
// fixed header block on every write (see configHeader). Users who edit
// by hand get their annotations rewritten - a known limitation, flagged
// in QUESTIONS.md. The trade-off is one round-trip through yaml.v3's
// shape-only marshaller rather than the Node-tree-rewriting path.
type Config struct {
	Providers  map[string]ProviderConfig `yaml:"providers"`
	Budget     BudgetConfig              `yaml:"budget"`
	PolicyPath string                    `yaml:"policy_path"`
	Memory     MemoryConfig              `yaml:"memory"`
	Chat       ChatConfig                `yaml:"chat,omitempty"`
	Server     ServerConfig              `yaml:"server"`
	MCP        MCPConfig                 `yaml:"mcp,omitempty"`

	// path remembers where this Config was loaded from so Save can
	// round-trip without the caller tracking it separately.
	path string `yaml:"-"`
}

// ChatConfig controls the chat persistence store. Omitted = default
// SQLite path ~/.hippo/chats.db. Set DBPath=":memory:" for ephemeral
// runs or an absolute path for a custom location.
type ChatConfig struct {
	DBPath string `yaml:"db_path,omitempty"`
}

// MCPConfig holds the user-declared Model Context Protocol server
// connections. Each entry is an MCPServerConfig.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers,omitempty"`
}

// MCPServerConfig describes one MCP server. The Name doubles as the
// human label and, when Prefix is empty and the entry was added via
// the web UI, the default prefix applied to the server's tool names.
// Library callers using mcp.Connect directly control Prefix via the
// mcp package's WithPrefix option; this struct only feeds the web UI
// path.
type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"`
	Command   []string          `yaml:"command,omitempty"`
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Prefix    string            `yaml:"prefix,omitempty"`
	Enabled   bool              `yaml:"enabled"`
}

// ProviderConfig is the per-provider settings block.
type ProviderConfig struct {
	APIKey       string `yaml:"api_key,omitempty"`
	BaseURL      string `yaml:"base_url,omitempty"`
	DefaultModel string `yaml:"default_model"`
	Enabled      bool   `yaml:"enabled"`
}

// BudgetConfig caps total spend. CeilingUSD <= 0 means unlimited.
type BudgetConfig struct {
	CeilingUSD float64 `yaml:"ceiling_usd"`
}

// MemoryConfig controls the memory store backing the Brain. Nested
// blocks configure the embedder, decay tuning, and prune policy;
// omitting them falls back to hippo's shipping defaults so existing
// v0.1.0 configs keep working.
type MemoryConfig struct {
	Enabled  bool             `yaml:"enabled"`
	DBPath   string           `yaml:"db_path"`
	Embedder EmbedderConfig   `yaml:"embedder,omitempty"`
	Prune    PruneConfigBlock `yaml:"prune,omitempty"`
}

// EmbedderConfig describes which embedder to use and how aggressively
// the backfill worker runs. Provider currently accepts only "ollama";
// more backends slot in later without breaking the shape.
type EmbedderConfig struct {
	Provider           string `yaml:"provider,omitempty"`
	Model              string `yaml:"model,omitempty"`
	BaseURL            string `yaml:"base_url,omitempty"`
	BackfillBatchSize  int    `yaml:"backfill_batch_size,omitempty"`
	BackfillIntervalS  int    `yaml:"backfill_interval_seconds,omitempty"`
}

// PruneConfigBlock is the YAML shape that maps onto
// memory/sqlite.PruneConfig. Stored as hours/seconds primitives so
// hand-editing isn't confusing.
type PruneConfigBlock struct {
	WorkingMaxAgeHours       int     `yaml:"working_max_age_hours,omitempty"`
	EpisodicMaxAgeHours      int     `yaml:"episodic_max_age_hours,omitempty"`
	EpisodicImportanceCutoff float64 `yaml:"episodic_importance_cutoff,omitempty"`
	AutoPruneIntervalMinutes int     `yaml:"auto_prune_interval_minutes,omitempty"`
}

// ServerConfig controls the HTTP listener. AuthToken is required only
// when Addr binds to a non-localhost address.
type ServerConfig struct {
	Addr      string `yaml:"addr"`
	AuthToken string `yaml:"auth_token"`
}

// DefaultConfigPath is the location hippo uses when no --config flag is
// supplied. Callers should call ExpandPath on whatever path they end up
// using so "~" survives a fresh shell.
const DefaultConfigPath = "~/.hippo/config.yaml"

// configHeader is the comment block Save writes at the top of every
// config.yaml. It survives hand-edits because Save strips the previous
// header before regenerating.
const configHeader = `# hippo config file
# Edit this file directly OR use the web UI at http://127.0.0.1:7844/config
# Keys are stored here in plain text. This file is mode 0600.
`

// DefaultConfig returns a fresh Config with the shipping defaults:
// every provider present but disabled, no credentials, localhost bind,
// memory enabled. Callers mutate this then Save.
func DefaultConfig() *Config {
	return &Config{
		Providers: map[string]ProviderConfig{
			"anthropic": {
				DefaultModel: "claude-haiku-4-5",
				Enabled:      false,
			},
			"openai": {
				DefaultModel: "gpt-5-nano",
				Enabled:      false,
			},
			"ollama": {
				BaseURL:      "http://localhost:11434",
				DefaultModel: "llama3.3:70b",
				Enabled:      false,
			},
		},
		Budget: BudgetConfig{CeilingUSD: 10.00},
		Memory: MemoryConfig{
			Enabled: true,
			DBPath:  "~/.hippo/memory.db",
		},
		Server: ServerConfig{
			Addr: "127.0.0.1:7844",
		},
	}
}

// Load reads and parses a Config YAML file. The path may start with "~"
// which is expanded to the current user's home.
func Load(path string) (*Config, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return nil, fmt.Errorf("web: expand %s: %w", path, err)
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("web: read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("web: parse config: %w", err)
	}
	c.path = expanded
	c.applyDefaults()
	return &c, nil
}

// applyDefaults fills in blanks so a partial config still boots. Does
// not mutate user-set values.
func (c *Config) applyDefaults() {
	if c.Providers == nil {
		c.Providers = map[string]ProviderConfig{}
	}
	if c.Server.Addr == "" {
		c.Server.Addr = "127.0.0.1:7844"
	}
}

// Save writes the config back to its original path atomically. The
// write goes to path.tmp, is fsynced, then renamed over the target so
// a crash mid-write cannot leave a truncated file.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("web: Config.Save: path not set")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("web: mkdir: %w", err)
	}

	body, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("web: marshal config: %w", err)
	}
	out := append([]byte(configHeader), '\n')
	out = append(out, body...)

	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("web: open tmp: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("web: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("web: sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("web: close tmp: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("web: rename tmp: %w", err)
	}
	return nil
}

// Path returns the absolute path this Config was loaded from.
func (c *Config) Path() string { return c.path }

// SetPath changes the target path for subsequent Save calls. Used by
// "init" flows that construct a DefaultConfig in memory and then
// commit it to disk at a user-chosen location.
func (c *Config) SetPath(p string) error {
	expanded, err := ExpandPath(p)
	if err != nil {
		return err
	}
	c.path = expanded
	return nil
}

// Validate sanity-checks the Config. Providers that are enabled must
// name a default_model. Enabled providers with empty credentials are
// allowed (Ollama has none; cloud providers may be partly configured
// while the user is in the middle of editing).
func (c *Config) Validate() error {
	for name, p := range c.Providers {
		if !p.Enabled {
			continue
		}
		if p.DefaultModel == "" {
			return fmt.Errorf("web: provider %q is enabled but default_model is empty", name)
		}
	}
	for i, s := range c.MCP.Servers {
		if !s.Enabled {
			continue
		}
		if s.Name == "" {
			return fmt.Errorf("web: MCP server #%d: name is empty", i)
		}
		switch s.Transport {
		case "stdio":
			if len(s.Command) == 0 {
				return fmt.Errorf("web: MCP server %q: stdio transport requires a command", s.Name)
			}
		case "http":
			if s.URL == "" {
				return fmt.Errorf("web: MCP server %q: http transport requires a URL", s.Name)
			}
		default:
			return fmt.Errorf("web: MCP server %q: unknown transport %q", s.Name, s.Transport)
		}
	}
	return nil
}

// EnabledProviders returns the names of providers with Enabled=true in
// a deterministic sort order (for UI render stability).
func (c *Config) EnabledProviders() []string {
	out := make([]string, 0, len(c.Providers))
	for name, p := range c.Providers {
		if p.Enabled {
			out = append(out, name)
		}
	}
	// Small map; tiny n insertion sort keeps one dependency-free path.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ExpandPath expands a leading "~" to the user's home directory.
// Other paths pass through unchanged.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

// InitConfig writes a fresh DefaultConfig to path (expanding ~). Refuses
// to overwrite an existing file. Used by `hippo init` and by `hippo
// serve` when the requested config is missing.
func InitConfig(path string) (*Config, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(expanded); err == nil {
		return nil, fmt.Errorf("web: %s already exists", expanded)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o700); err != nil {
		return nil, fmt.Errorf("web: mkdir: %w", err)
	}
	c := DefaultConfig()
	c.path = expanded
	if err := c.Save(); err != nil {
		return nil, err
	}
	return c, nil
}
