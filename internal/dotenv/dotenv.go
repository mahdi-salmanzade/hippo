// Package dotenv is a minimal .env file loader for local development.
// It is intentionally in internal/: this is not part of hippo's public
// surface. Tests and examples use it so contributors don't have to
// source the file into their shell manually.
//
// Dotenv semantics: values already set in the process environment are
// preserved (CI and production win over a developer's .env). Missing
// .env is not an error — Load returns nil so callers can invoke it
// unconditionally.
package dotenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Load walks upward from the current working directory looking for a
// .env file. When found, it reads KEY=value pairs and sets them in the
// process environment using os.Setenv, skipping any key that is
// already set. Returns nil if no .env is found anywhere between CWD
// and the filesystem root.
func Load() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		path := filepath.Join(dir, ".env")
		if _, err := os.Stat(path); err == nil {
			return loadFile(path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// loadFile reads one .env file. Supports KEY=value, blank lines,
// #-comments, and single/double-quoted values. Malformed lines are
// silently skipped; this is a dev convenience, not a strict parser.
func loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 {
			first, last := value[0], value[n-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : n-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return scanner.Err()
}
