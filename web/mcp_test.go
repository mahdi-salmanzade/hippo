package web

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestBundleSkipsUnreachableMCPServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if _, err := InitConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Memory.Enabled = false
	// Enable anthropic with a placeholder key so Brain construction
	// succeeds (BuildBrain requires at least one provider before it
	// attempts router/budget wiring); the bogus MCP entry is then the
	// thing we're actually exercising.
	ap := cfg.Providers["anthropic"]
	ap.APIKey = "sk-placeholder"
	ap.Enabled = true
	ap.DefaultModel = "claude-haiku-4-5"
	cfg.Providers["anthropic"] = ap

	cfg.MCP.Servers = []MCPServerConfig{
		{
			Name:      "bogus",
			Transport: "stdio",
			Command:   []string{"/definitely/not/a/real/binary-xyzzy"},
			Enabled:   true,
		},
	}

	bundle, err := BuildBrain(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("BuildBrain returned error: %v", err)
	}
	if bundle.Brain == nil {
		t.Fatal("Brain should still build with zero MCP clients")
	}
	if len(bundle.Warnings) == 0 {
		t.Error("expected a warning for the unreachable MCP server")
	}
	bundle.Close()
}

func TestParseMCPFormAppendsNewServer(t *testing.T) {
	// A GET form with mcp_add=1 and no existing rows should yield
	// one disabled default entry.
	req := fakeForm(map[string]string{
		"mcp_add": "1",
	})
	cfg := parseMCPForm(req)
	if len(cfg.Servers) != 1 {
		t.Fatalf("Servers = %d", len(cfg.Servers))
	}
	if cfg.Servers[0].Enabled {
		t.Error("default appended entry should be disabled")
	}
}

func TestParseMCPFormRoundTripsRow(t *testing.T) {
	req := fakeForm(map[string]string{
		"mcp_name_0":      "my-server",
		"mcp_transport_0": "stdio",
		"mcp_command_0":   `/bin/echo "hello world"`,
		"mcp_prefix_0":    "srv",
		"mcp_enabled_0":   "on",
	})
	cfg := parseMCPForm(req)
	if len(cfg.Servers) != 1 {
		t.Fatalf("Servers = %d", len(cfg.Servers))
	}
	s := cfg.Servers[0]
	if s.Name != "my-server" || s.Prefix != "srv" || !s.Enabled {
		t.Errorf("got %+v", s)
	}
	if len(s.Command) != 2 || s.Command[0] != "/bin/echo" || s.Command[1] != "hello world" {
		t.Errorf("Command = %v", s.Command)
	}
}

func TestParseMCPFormDeletesByIndex(t *testing.T) {
	req := fakeForm(map[string]string{
		"mcp_name_0":      "keep",
		"mcp_transport_0": "http",
		"mcp_command_0":   "http://example.com/mcp",
		"mcp_name_1":      "drop",
		"mcp_transport_1": "http",
		"mcp_command_1":   "http://drop.me",
		"mcp_delete":      "1",
	})
	cfg := parseMCPForm(req)
	if len(cfg.Servers) != 1 {
		t.Fatalf("Servers = %d", len(cfg.Servers))
	}
	if cfg.Servers[0].Name != "keep" {
		t.Errorf("wrong server survived: %+v", cfg.Servers[0])
	}
}
