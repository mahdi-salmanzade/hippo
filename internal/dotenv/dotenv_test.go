package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileParsesAndSets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := `# comment
KEY_PLAIN=plain
KEY_QUOTED="quoted value"
KEY_SINGLE='single quoted'
KEY_WITH_EQ=a=b=c

# next line is malformed and must be skipped
NOEQUALS
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ensure a clean slate for keys we set.
	keys := []string{"KEY_PLAIN", "KEY_QUOTED", "KEY_SINGLE", "KEY_WITH_EQ"}
	for _, k := range keys {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	if err := loadFile(path); err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if got := os.Getenv("KEY_PLAIN"); got != "plain" {
		t.Errorf("KEY_PLAIN = %q, want %q", got, "plain")
	}
	if got := os.Getenv("KEY_QUOTED"); got != "quoted value" {
		t.Errorf("KEY_QUOTED = %q, want %q", got, "quoted value")
	}
	if got := os.Getenv("KEY_SINGLE"); got != "single quoted" {
		t.Errorf("KEY_SINGLE = %q, want %q", got, "single quoted")
	}
	if got := os.Getenv("KEY_WITH_EQ"); got != "a=b=c" {
		t.Errorf("KEY_WITH_EQ = %q, want %q", got, "a=b=c")
	}
}

func TestLoadFilePreservesExistingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("PRESET=from_file"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PRESET", "from_process")
	t.Cleanup(func() { os.Unsetenv("PRESET") })

	if err := loadFile(path); err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if got := os.Getenv("PRESET"); got != "from_process" {
		t.Errorf("PRESET = %q, want %q (process env must win)", got, "from_process")
	}
}

func TestLoadFindsNearestUpward(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".env")
	if err := os.WriteFile(path, []byte("WALKUP=ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("WALKUP")
	t.Cleanup(func() { os.Unsetenv("WALKUP") })

	if err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("WALKUP"); got != "ok" {
		t.Errorf("WALKUP = %q, want %q", got, "ok")
	}
}

func TestLoadNoEnvFileIsNoOp(t *testing.T) {
	// Chdir into an empty temp dir with no .env anywhere above it.
	// On most systems at least / has no .env; if a user dropped one
	// at root, this test would be flaky - but that's an exotic
	// configuration and not worth defending against.
	dir := t.TempDir()
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := Load(); err != nil {
		t.Errorf("Load on empty tree returned error: %v", err)
	}
}
