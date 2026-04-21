package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildEchoServer compiles examples/mcp/echo_server into a temp
// location so stdio tests can spawn it without relying on `go run`
// which is slow and also emits "go" banner noise on stderr. The
// returned path is cleaned up by t.Cleanup.
func buildEchoServer(t *testing.T) string {
	t.Helper()

	// Repo root is three directories up from this test file:
	// mcp/ → hippo/ (root).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Dir(wd)

	bin := filepath.Join(t.TempDir(), "echo_server")
	cmd := exec.Command("go", "build", "-o", bin, "./examples/mcp/echo_server")
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build echo_server: %v", err)
	}
	return bin
}

func TestStdioIntegrationEchoServer(t *testing.T) {
	bin := buildEchoServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Connect(ctx, []string{bin}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	if c.Name() != "echo" {
		t.Errorf("server name = %q; want 'echo'", c.Name())
	}
	tools := c.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %d; want 2", len(tools))
	}

	byName := map[string]bool{}
	for _, tt := range tools {
		byName[tt.Name()] = true
	}
	if !byName["echo"] || !byName["add"] {
		t.Errorf("unexpected tool set: %v", byName)
	}

	// Round-trip the echo tool.
	var echoTool = tools[0]
	for _, tt := range tools {
		if tt.Name() == "echo" {
			echoTool = tt
		}
	}
	res, err := echoTool.Execute(ctx, json.RawMessage(`{"text":"hello world"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("IsError; content=%q", res.Content)
	}
	if res.Content != "hello world" {
		t.Errorf("Content = %q", res.Content)
	}
}

func TestStdioSubprocessCrashDisconnects(t *testing.T) {
	// Spawn /bin/cat, which stays alive forever reading stdin. It
	// won't respond to initialize, so Connect will fail. That's fine
	// for this test - we just want to exercise the error path.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Connect(ctx, []string{"/bin/cat"}, WithLogger(discardLogger()), WithInitTimeout(200*time.Millisecond))
	if err == nil {
		t.Fatal("expected init timeout")
	}
}
