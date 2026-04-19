package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// fakeForm builds an *http.Request with the given fields set in
// r.Form for tests that exercise form parsing directly.
func fakeForm(fields map[string]string) *http.Request {
	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	return req
}

// freshServer builds a Server wired to a throw-away config file. Used
// by handler tests — the server isn't actually Start'd because the
// tests exercise the mux directly via httptest.
func freshServer(t *testing.T) *Server {
	t.Helper()
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
	srv, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestConfigGETRenders(t *testing.T) {
	srv := freshServer(t)
	req := httptest.NewRequest("GET", "/config", nil)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Anthropic") || !strings.Contains(body, "API key") {
		t.Errorf("missing expected content:\n%s", body)
	}
}

func TestConfigPOSTSavesAndRebuilds(t *testing.T) {
	srv := freshServer(t)
	form := url.Values{}
	form.Set("anthropic_api_key", "") // keep
	form.Set("anthropic_default_model", "claude-haiku-4-5")
	form.Set("openai_default_model", "gpt-5-nano")
	form.Set("ollama_base_url", "http://localhost:11434")
	form.Set("ollama_default_model", "llama3.3:70b")
	form.Set("budget_ceiling_usd", "5")
	req := httptest.NewRequest("POST", "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 303 {
		t.Fatalf("code = %d", w.Code)
	}
	if srv.cfg.Budget.CeilingUSD != 5 {
		t.Errorf("budget not saved: %v", srv.cfg.Budget.CeilingUSD)
	}
}

func TestSpendGETRenders(t *testing.T) {
	srv := freshServer(t)
	srv.state.Record(CallRecord{Provider: "anthropic", Task: "reason", CostUSD: 0.25, Prompt: "hi", Timestamp: time.Now()})
	req := httptest.NewRequest("GET", "/spend", nil)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "anthropic") {
		t.Errorf("missing row")
	}
}

func TestRecentCallsFragment(t *testing.T) {
	srv := freshServer(t)
	srv.state.Record(CallRecord{Provider: "openai", Task: "generate", Timestamp: time.Now()})
	req := httptest.NewRequest("GET", "/api/recent-calls", nil)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "openai") {
		t.Errorf("fragment missing row:\n%s", w.Body.String())
	}
}

func TestProvidersJSON(t *testing.T) {
	srv := freshServer(t)
	req := httptest.NewRequest("GET", "/api/providers", nil)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("want 3 providers, got %d", len(list))
	}
}

func TestPolicyPOSTRejectsInvalidYAML(t *testing.T) {
	srv := freshServer(t)
	form := url.Values{"policy_yaml": {"::: not yaml :::"}}
	req := httptest.NewRequest("POST", "/policy", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", w.Code)
	}
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "hippo_flash_err" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected flash_err cookie on invalid YAML")
	}
}

// fakeStreamBrainBundle builds a BrainBundle whose Brain has been
// replaced with a trivial provider emitting deterministic chunks.
// Used by TestChatStreamSSEFlow.
type fakeProvider struct{ privacy hippo.PrivacyTier }

func (f *fakeProvider) Name() string                                 { return "fake" }
func (f *fakeProvider) Models() []hippo.ModelInfo                    { return nil }
func (f *fakeProvider) Privacy() hippo.PrivacyTier                   { return f.privacy }
func (f *fakeProvider) EstimateCost(c hippo.Call) (float64, error)   { return 0, nil }
func (f *fakeProvider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	return &hippo.Response{Text: "ok", Provider: "fake", Model: "fake-1"}, nil
}
func (f *fakeProvider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	ch := make(chan hippo.StreamChunk, 4)
	go func() {
		defer close(ch)
		ch <- hippo.StreamChunk{Type: hippo.StreamChunkText, Delta: "hello "}
		ch <- hippo.StreamChunk{Type: hippo.StreamChunkText, Delta: "world"}
		u := hippo.Usage{InputTokens: 5, OutputTokens: 2}
		ch <- hippo.StreamChunk{
			Type: hippo.StreamChunkUsage, Usage: &u, CostUSD: 0.00001,
			Provider: "fake", Model: "fake-1",
		}
	}()
	return ch, nil
}

func TestChatStreamSSEFlow(t *testing.T) {
	srv := freshServer(t)
	brain, err := hippo.New(hippo.WithProvider(&fakeProvider{}))
	if err != nil {
		t.Fatal(err)
	}
	srv.ReplaceBundle(&BrainBundle{Brain: brain})

	form := url.Values{"prompt": {"hi"}, "task": {"generate"}}
	req := httptest.NewRequest("POST", "/chat", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("POST /chat code = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Session == "" {
		t.Fatal("empty session")
	}

	streamReq := httptest.NewRequest("GET", "/chat/stream?session="+out.Session, nil)
	sw := httptest.NewRecorder()
	srv.routes().ServeHTTP(sw, streamReq)
	if sw.Code != 200 {
		t.Fatalf("stream code = %d", sw.Code)
	}
	events := parseSSE(t, sw.Body.String())
	var got []string
	var delta string
	for _, e := range events {
		got = append(got, e.name)
		if e.name == "delta" {
			delta += e.data
		}
	}
	if delta != "hello world" {
		t.Errorf("delta = %q; want 'hello world'", delta)
	}
	wantContains := []string{"delta", "usage", "done"}
	for _, w := range wantContains {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q event; got %v", w, got)
		}
	}
}

type sseEvent struct{ name, data string }

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	var cur sseEvent
	r := bufio.NewScanner(strings.NewReader(body))
	r.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for r.Scan() {
		line := r.Text()
		if line == "" {
			if cur.name != "" {
				events = append(events, cur)
				cur = sseEvent{}
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			cur.name = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			if cur.data != "" {
				cur.data += "\n"
			}
			cur.data += strings.TrimPrefix(line, "data: ")
		}
	}
	if err := r.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("scan: %v", err)
	}
	if cur.name != "" {
		events = append(events, cur)
	}
	return events
}
