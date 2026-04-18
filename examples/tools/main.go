// Command tools is the canonical Pass 8 demo: register three
// home-grown tools with hippo and watch the streaming tool loop work
// end-to-end. hippo ships no tools of its own — everything you need
// to reproduce is in this file.
//
// Run with:
//
//	ANTHROPIC_API_KEY=... go run ./examples/tools
//	OPENAI_API_KEY=...    go run ./examples/tools openai
//
// Tools demonstrated:
//
//   - now: returns the current RFC3339 timestamp.
//   - calc: evaluates a tiny arithmetic expression ("2 + 3*4").
//   - wordcount: counts whitespace-separated words in a string.
//
// The prompt forces a pair of tool invocations (likely in parallel on
// supported models). Streaming is the default because it lets the
// user see each StreamChunkToolCall and StreamChunkToolResult as they
// happen, which makes the loop concrete.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
)

// --- Tools ----------------------------------------------------------

type nowTool struct{}

func (nowTool) Name() string        { return "now" }
func (nowTool) Description() string { return "Returns the current date and time as an RFC3339 string." }
func (nowTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (nowTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	return hippo.ToolResult{Content: time.Now().UTC().Format(time.RFC3339)}, nil
}

type calcTool struct{}

func (calcTool) Name() string { return "calc" }
func (calcTool) Description() string {
	return "Evaluates a simple arithmetic expression with +, -, *, /. Example: '2 + 3*4'."
}
func (calcTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"expression":{"type":"string","description":"The arithmetic expression."}},
		"required":["expression"],
		"additionalProperties":false
	}`)
}
func (calcTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	var in struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return hippo.ToolResult{Content: "bad args: " + err.Error(), IsError: true}, nil
	}
	v, err := evalExpr(in.Expression)
	if err != nil {
		return hippo.ToolResult{Content: "calc error: " + err.Error(), IsError: true}, nil
	}
	return hippo.ToolResult{Content: strconv.FormatFloat(v, 'f', -1, 64)}, nil
}

type wordcountTool struct{}

func (wordcountTool) Name() string        { return "wordcount" }
func (wordcountTool) Description() string { return "Counts whitespace-separated words in the given string." }
func (wordcountTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"text":{"type":"string"}},
		"required":["text"],
		"additionalProperties":false
	}`)
}
func (wordcountTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return hippo.ToolResult{Content: "bad args: " + err.Error(), IsError: true}, nil
	}
	return hippo.ToolResult{Content: strconv.Itoa(len(strings.Fields(in.Text)))}, nil
}

// evalExpr is a 3-operator recursive descent parser — enough for a
// demo, not production. Supports +, -, *, /, parens, numbers.
func evalExpr(s string) (float64, error) {
	p := &exprParser{s: strings.TrimSpace(s)}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.pos < len(p.s) {
		return 0, fmt.Errorf("unexpected trailing %q", p.s[p.pos:])
	}
	return v, nil
}

type exprParser struct {
	s   string
	pos int
}

func (p *exprParser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for p.peek() == '+' || p.peek() == '-' {
		op := p.next()
		right, err := p.parseTerm()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			left += right
		} else {
			left -= right
		}
	}
	return left, nil
}

func (p *exprParser) parseTerm() (float64, error) {
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for p.peek() == '*' || p.peek() == '/' {
		op := p.next()
		right, err := p.parseFactor()
		if err != nil {
			return 0, err
		}
		if op == '*' {
			left *= right
		} else {
			if right == 0 {
				return 0, errors.New("divide by zero")
			}
			left /= right
		}
	}
	return left, nil
}

func (p *exprParser) parseFactor() (float64, error) {
	p.skipSpaces()
	if p.peek() == '(' {
		p.next()
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		p.skipSpaces()
		if p.peek() != ')' {
			return 0, errors.New("missing close paren")
		}
		p.next()
		return v, nil
	}
	start := p.pos
	for p.pos < len(p.s) && (isDigit(p.s[p.pos]) || p.s[p.pos] == '.') {
		p.pos++
	}
	if start == p.pos {
		return 0, fmt.Errorf("expected number at pos %d", p.pos)
	}
	return strconv.ParseFloat(p.s[start:p.pos], 64)
}

func (p *exprParser) peek() byte {
	p.skipSpaces()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *exprParser) next() byte {
	p.skipSpaces()
	if p.pos >= len(p.s) {
		return 0
	}
	ch := p.s[p.pos]
	p.pos++
	return ch
}

func (p *exprParser) skipSpaces() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// --- Main -----------------------------------------------------------

func main() {
	_ = dotenv.Load()

	which := "anthropic"
	if len(os.Args) > 1 {
		which = os.Args[1]
	}

	var p hippo.Provider
	var err error
	switch which {
	case "openai":
		p, err = openai.New(
			openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
			openai.WithModel("gpt-5-nano"),
		)
	default:
		p, err = anthropic.New(
			anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
			anthropic.WithModel("claude-haiku-4-5"),
		)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	b, err := hippo.New(
		hippo.WithProvider(p),
		hippo.WithTools(nowTool{}, calcTool{}, wordcountTool{}),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	prompt := "What is the current time, and what does 17 * 23 + 4 equal? " +
		"Use the now and calc tools. Reply with a single sentence summarising both."

	ch, err := b.Stream(ctx, hippo.Call{
		Task:      hippo.TaskReason,
		Prompt:    prompt,
		MaxTokens: 400,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "stream open:", err)
		os.Exit(1)
	}

	for chunk := range ch {
		switch chunk.Type {
		case hippo.StreamChunkText:
			fmt.Print(chunk.Delta)
		case hippo.StreamChunkThinking:
			fmt.Fprint(os.Stderr, chunk.Delta)
		case hippo.StreamChunkToolCall:
			fmt.Fprintf(os.Stderr, "\n→ calling %s(%s)\n", chunk.ToolCall.Name, chunk.ToolCall.Arguments)
		case hippo.StreamChunkToolResult:
			fmt.Fprintf(os.Stderr, "← result (%s): %s\n", chunk.ToolCallID, chunk.ToolResult.Content)
		case hippo.StreamChunkError:
			fmt.Fprintln(os.Stderr, "\nstream error:", chunk.Error)
			os.Exit(1)
		case hippo.StreamChunkUsage:
			fmt.Printf("\n\n[%s/%s · %d in / %d out · $%.6f]\n",
				chunk.Provider, chunk.Model,
				chunk.Usage.InputTokens, chunk.Usage.OutputTokens,
				chunk.CostUSD)
		}
	}
}
