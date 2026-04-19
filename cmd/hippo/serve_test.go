package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mahdi-salmanzade/hippo/web"
)

// TestServeRefusesPublicBindWithoutToken exercises BindGuard via the
// CLI wiring: runServe should surface the bind-guard error rather than
// start listening. We can't actually call runServe because it blocks on
// a signal; instead, mimic its bind-merge logic and call web.New.
func TestServeRefusesPublicBindWithoutToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if _, err := web.InitConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := web.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Addr = "0.0.0.0:0"
	if _, err := web.New(cfg); err == nil {
		t.Fatal("want bind-guard error; got nil")
	}
}

func TestInitRefusesOverwriteCLI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if err := runInit([]string{"--config", path}); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"--config", path}); err == nil {
		t.Fatal("second init; want error")
	}
}

func TestVersionPrints(t *testing.T) {
	// Capture stdout around runVersion.
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	if err := runVersion(nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "hippo") {
		t.Errorf("version output missing banner: %q", got)
	}
}
