package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandPathTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	expanded, err := ExpandPath("~/foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	if expanded != filepath.Join(home, "foo/bar") {
		t.Errorf("got %q; want %s/foo/bar", expanded, home)
	}
	if out, _ := ExpandPath("/absolute"); out != "/absolute" {
		t.Errorf("absolute passthrough got %q", out)
	}
}

func TestInitConfigCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	c, err := InitConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Path() != path {
		t.Errorf("Path() = %q; want %q", c.Path(), path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v; want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "providers:") {
		t.Errorf("config missing providers section:\n%s", data)
	}
}

func TestInitConfigRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if _, err := InitConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := InitConfig(path); err == nil {
		t.Fatal("second InitConfig; want error")
	}
}

func TestLoadParsesWrittenConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if _, err := InitConfig(path); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Providers["anthropic"]; !ok {
		t.Errorf("anthropic missing: %#v", c.Providers)
	}
	if c.Server.Addr == "" {
		t.Error("server.addr empty after Load")
	}
}

func TestSaveAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	c, err := InitConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate and save again.
	pc := c.Providers["anthropic"]
	pc.APIKey = "sk-test"
	pc.Enabled = true
	c.Providers["anthropic"] = pc
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	// Confirm no leftover .tmp file.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("leftover .tmp file after save")
	}
	// Confirm mode preserved.
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode drifted to %v", info.Mode().Perm())
	}
	// Re-load and verify.
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Providers["anthropic"].APIKey != "sk-test" {
		t.Errorf("api_key round-trip failed: %+v", c2.Providers["anthropic"])
	}
}

func TestValidateCatchesMissingDefaultModel(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"anthropic": {APIKey: "k", Enabled: true},
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() with enabled+no-model; want error")
	}
}

func TestValidateHappyPath(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"anthropic": {APIKey: "k", Enabled: true, DefaultModel: "claude-haiku-4-5"},
		"openai":    {Enabled: false},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v; want nil", err)
	}
}

func TestEnabledProvidersSortedAndFiltered(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"ollama":    {Enabled: true},
		"anthropic": {Enabled: false},
		"openai":    {Enabled: true},
	}}
	got := c.EnabledProviders()
	want := []string{"ollama", "openai"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v; want %v", got, want)
		}
	}
}
