// Command routing demonstrates policy-based routing: attach multiple
// providers and a YAML router, then issue calls with different Task
// kinds to see the router pick different backends.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/ollama"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
	yamlrouter "github.com/mahdi-salmanzade/hippo/router/yaml"
)

func main() {
	r, err := yamlrouter.Load("policy.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ap, err := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	b, err := hippo.New(
		hippo.WithProvider(ap),
		hippo.WithProvider(openai.New(os.Getenv("OPENAI_API_KEY"))),
		hippo.WithProvider(ollama.New()),
		hippo.WithRouter(r),
		hippo.WithBudget(budget.Daily(1.00)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ctx := context.Background()

	// Cheap, fast → policy routes to a small model.
	_, _ = b.Call(ctx, hippo.Call{
		Task:   hippo.TaskClassify,
		Prompt: "Is this spam? 'Congrats you won a prize!'",
	})

	// Hard reasoning → policy routes to a frontier model.
	_, _ = b.Call(ctx, hippo.Call{
		Task:   hippo.TaskReason,
		Prompt: "Prove that sqrt(2) is irrational.",
	})

	// Sensitive → policy routes to Ollama (local only).
	_, _ = b.Call(ctx, hippo.Call{
		Task:    hippo.TaskProtect,
		Privacy: hippo.PrivacyLocalOnly,
		Prompt:  "Summarise these meeting notes: ...",
	})
}
