package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo/providers/ollama"
	"github.com/mahdi-salmanzade/hippo/web"
)

// runSetup inspects the machine for the prerequisites hippo needs to
// run with full functionality (embedder-backed memory) and prints a
// pass/fail line per check. With --fix it will pull the embedder model
// and write a memory.embedder block into the config. It will NOT
// install the Ollama daemon itself — that's platform-specific and
// typically needs admin rights; we print the exact install command.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	configPath := fs.String("config", web.DefaultConfigPath, "path to config file")
	model := fs.String("model", ollama.DefaultEmbeddingModel, "embedder model to use")
	fix := fs.Bool("fix", false, "pull the embedder model and patch the config when possible")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println("hippo setup — checking prerequisites")
	fmt.Println()

	// 1. Config file.
	cfg, created, err := ensureConfig(*configPath)
	if err != nil {
		return err
	}
	if created {
		printOK("config", "created "+cfg.Path())
	} else {
		printOK("config", cfg.Path())
	}

	// 2. Ollama binary on PATH. Missing binary is not fatal — a user
	//    on a machine where `ollama` lives off-PATH can still reach a
	//    daemon over the network, so we only surface the gap.
	ollamaOnPath := true
	if _, err := exec.LookPath("ollama"); err != nil {
		ollamaOnPath = false
		printMissing("ollama binary", "not on PATH")
		fmt.Printf("     install: %s\n", installHint())
	} else {
		printOK("ollama binary", "on PATH")
	}

	// 3. Ollama daemon reachable.
	baseURL := cfg.Providers["ollama"].BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	tags, err := fetchOllamaTags(baseURL)
	daemonUp := err == nil
	if !daemonUp {
		printMissing("ollama daemon", fmt.Sprintf("unreachable at %s: %v", baseURL, err))
		fmt.Println("     start it: ollama serve  (or: brew services start ollama)")
	} else {
		printOK("ollama daemon", "reachable at "+baseURL)
	}

	// 4. Embedder model present. Only checkable when the daemon answered.
	modelPresent := false
	if daemonUp {
		modelPresent = containsModel(tags, *model)
		switch {
		case modelPresent:
			printOK("embedder model", *model+" installed")
		case *fix && ollamaOnPath:
			fmt.Printf("[..] embedder model   pulling %s (this downloads a few hundred MB)\n", *model)
			if err := pullModel(*model); err != nil {
				printError("embedder model", "pull failed: "+err.Error())
			} else {
				printOK("embedder model", "pulled "+*model)
				modelPresent = true
			}
		default:
			printMissing("embedder model", *model+" not installed")
			fmt.Printf("     pull it: ollama pull %s\n", *model)
		}
	}

	// 5. Embedder wired in config. Two valid states: explicit embedder
	//    block, or the Ollama provider enabled (BuildBrain infers the
	//    embedder from it when no block is set).
	ec := cfg.Memory.Embedder
	wired := ec.Provider == "ollama" ||
		(ec.Provider == "" && cfg.Providers["ollama"].Enabled)
	if wired {
		printOK("embedder config", "wired in "+cfg.Path())
	} else if *fix {
		cfg.Memory.Embedder.Provider = "ollama"
		cfg.Memory.Embedder.Model = *model
		cfg.Memory.Embedder.BaseURL = baseURL
		if err := cfg.Save(); err != nil {
			printError("embedder config", "write failed: "+err.Error())
		} else {
			printOK("embedder config", "wrote memory.embedder to "+cfg.Path())
			wired = true
		}
	} else {
		printMissing("embedder config", "no memory.embedder block and ollama provider disabled")
		fmt.Printf("     add this under memory: in %s\n", cfg.Path())
		fmt.Println("         embedder:")
		fmt.Println("             provider: ollama")
		fmt.Printf("             model: %s\n", *model)
		fmt.Printf("             base_url: %s\n", baseURL)
	}

	fmt.Println()
	if daemonUp && modelPresent && wired {
		fmt.Println("Ready. Next: hippo serve --open")
	} else if !*fix {
		fmt.Println("Some checks failed. Re-run with --fix to apply what hippo can do safely.")
	} else {
		fmt.Println("Re-run `hippo setup` after fixing the remaining items above.")
	}
	return nil
}

// ensureConfig returns the Config at path, creating one with defaults
// when the file is missing. The bool reports whether a fresh file was
// written.
func ensureConfig(path string) (*web.Config, bool, error) {
	cfg, err := web.Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if !errors.Is(unwrapPathErr(err), os.ErrNotExist) {
		return nil, false, err
	}
	cfg, err = web.InitConfig(path)
	if err != nil {
		return nil, false, fmt.Errorf("create config: %w", err)
	}
	return cfg, true, nil
}

// installHint returns a best-guess install command for the running OS.
// Keeps the advice one line — long prose belongs on ollama.com/download.
func installHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install ollama  (or download: https://ollama.com/download)"
	case "linux":
		return "curl -fsSL https://ollama.com/install.sh | sh"
	case "windows":
		return "download the installer from https://ollama.com/download"
	default:
		return "see https://ollama.com/download"
	}
}

// fetchOllamaTags queries /api/tags with a short timeout and returns
// the list of installed model names. Used both to probe that the
// daemon is up and to check whether the embedder model is present.
func fetchOllamaTags(baseURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// containsModel matches both the short user-facing name ("nomic-embed-text")
// and the tag-qualified form Ollama returns ("nomic-embed-text:latest").
func containsModel(tags []string, want string) bool {
	for _, t := range tags {
		if t == want || strings.HasPrefix(t, want+":") {
			return true
		}
	}
	return false
}

// pullModel shells out to `ollama pull`, streaming its progress bar
// directly to the user's terminal. Runs in the foreground because the
// download is the whole point of the call.
func pullModel(name string) error {
	cmd := exec.Command("ollama", "pull", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printOK(label, detail string)      { fmt.Printf("[ok] %-16s %s\n", label, detail) }
func printMissing(label, detail string) { fmt.Printf("[--] %-16s %s\n", label, detail) }
func printError(label, detail string)   { fmt.Printf("[!!] %-16s %s\n", label, detail) }
