// Command mcp_demo connects to a local MCP server (the bundled
// echo_server by default) and exposes its tools to an Anthropic-backed
// Brain. Run with ANTHROPIC_API_KEY set:
//
//	go run ./examples/mcp
//	go run ./examples/mcp --server "/path/to/your/mcp-server"
//
// The demo prompts the model to echo a phrase; the model picks the
// MCP `echo` tool, the tool runs remotely, and the final response
// quotes what was echoed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/mcp"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
)

func main() {
	_ = dotenv.Load()

	var serverFlag string
	flag.StringVar(&serverFlag, "server", "", "MCP server command (defaults to the bundled echo_server via go run)")
	flag.Parse()

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("set ANTHROPIC_API_KEY to run the demo")
	}

	command := []string{"go", "run", "./examples/mcp/echo_server"}
	if serverFlag != "" {
		command = strings.Fields(serverFlag)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := mcp.Connect(ctx, command, mcp.WithPrefix("echo"))
	if err != nil {
		log.Fatalf("mcp.Connect: %v", err)
	}
	defer client.Close()

	fmt.Printf("connected to MCP server %q with %d tools\n", client.Name(), len(client.Tools()))
	for _, t := range client.Tools() {
		fmt.Printf("  - %s: %s\n", t.Name(), t.Description())
	}

	provider, err := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
	if err != nil {
		log.Fatalf("anthropic: %v", err)
	}

	brain, err := hippo.New(
		hippo.WithProvider(provider),
		hippo.WithMCPClients(client),
	)
	if err != nil {
		log.Fatalf("hippo.New: %v", err)
	}
	defer brain.Close()

	resp, err := brain.Call(ctx, hippo.Call{
		Task:      hippo.TaskGenerate,
		Model:     "claude-haiku-4-5",
		Prompt:    `Use the echo_echo tool to echo the phrase "hippo speaks MCP". Then tell me what came back.`,
		MaxTokens: 300,
	})
	if err != nil {
		log.Fatalf("brain.Call: %v", err)
	}

	fmt.Println("\n=== final response ===")
	fmt.Println(resp.Text)
	fmt.Printf("\n(cost: $%.6f  tokens: %d→%d)\n",
		resp.CostUSD, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}
