package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// chatPageData is the render-time shape for chat.html.
type chatPageData struct {
	Providers []chatProviderView
	Tasks     []string
	ToolCount int
}

type chatProviderView struct {
	Name         string
	DisplayName  string
	Models       []string
	DefaultModel string
}

func (s *Server) handleChatGet(w http.ResponseWriter, r *http.Request) {
	var provs []chatProviderView
	for _, name := range []string{"anthropic", "openai", "ollama"} {
		pc, ok := s.cfg.Providers[name]
		if !ok || !pc.Enabled {
			continue
		}
		provs = append(provs, chatProviderView{
			Name:         name,
			DisplayName:  providerDisplayName(name),
			Models:       modelIDsFor(name),
			DefaultModel: pc.DefaultModel,
		})
	}
	tasks := []string{"classify", "reason", "generate", "protect"}
	toolCount := 0
	if b := s.Bundle(); b != nil {
		for _, c := range b.MCPClients {
			toolCount += len(c.Tools())
		}
	}
	s.render(w, "chat.html", pageData{
		Title:  "Chat",
		Active: "chat",
		Data: chatPageData{
			Providers: provs,
			Tasks:     tasks,
			ToolCount: toolCount,
		},
	})
}

// chatRequest is the POST /chat body (form-encoded).
type chatRequest struct {
	Prompt   string
	Provider string
	Model    string
	Task     hippo.TaskKind
	Memory   bool
	Tools    bool
}

// handleChatPost validates the incoming Call, stashes it in the
// session map keyed by a random id, and returns the id as JSON. The
// client then opens a GET /chat/stream?session=<id> connection.
func (s *Server) handleChatPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req := chatRequest{
		Prompt:   r.FormValue("prompt"),
		Provider: r.FormValue("provider"),
		Model:    r.FormValue("model"),
		Task:     hippo.TaskKind(r.FormValue("task")),
		Memory:   r.FormValue("memory") == "on",
		Tools:    r.FormValue("tools") == "on",
	}
	if req.Prompt == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}
	if req.Task == "" {
		req.Task = hippo.TaskGenerate
	}

	b := s.Bundle()
	if b == nil || b.Brain == nil {
		http.Error(w, "no brain configured", http.StatusServiceUnavailable)
		return
	}

	call := hippo.Call{
		Task:   req.Task,
		Prompt: req.Prompt,
		Model:  req.Model,
	}
	if req.Memory {
		call.UseMemory = hippo.MemoryScope{Mode: hippo.MemoryScopeRecent}
	}
	if req.Provider != "" {
		// A fixed provider bypasses the router — tell the Brain what
		// model to use and let it dispatch to the first registered
		// provider of that name. The router will still pick if Model
		// is empty; with a pinned model and provider the call goes
		// directly.
		call.Metadata = map[string]any{"ui_provider": req.Provider}
	}

	id, err := newSessionID()
	if err != nil {
		http.Error(w, "session id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.state.PutSession(id, &ChatSession{
		ID:        id,
		Call:      call,
		CreatedAt: time.Now(),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"session": id})
}

// handleChatStream opens an SSE connection and streams chunks from
// Brain.Stream to the client. The session id must match a live,
// unclaimed session or the request is rejected.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	sessID := r.URL.Query().Get("session")
	if sessID == "" {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}
	sess := s.state.TakeSession(sessID)
	if sess == nil {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	bundle := s.Bundle()
	if bundle == nil || bundle.Brain == nil {
		writeSSE(w, flusher, "error", "no brain configured")
		return
	}

	ctx := r.Context()
	start := time.Now()
	ch, err := bundle.Brain.Stream(ctx, sess.Call)
	if err != nil {
		writeSSE(w, flusher, "error", err.Error())
		return
	}

	var fullText string
	record := CallRecord{
		Timestamp: time.Now(),
		Task:      string(sess.Call.Task),
		Prompt:    sess.Call.Prompt,
	}

	for chunk := range ch {
		switch chunk.Type {
		case hippo.StreamChunkText:
			fullText += chunk.Delta
			writeSSE(w, flusher, "delta", chunk.Delta)
		case hippo.StreamChunkThinking:
			writeSSE(w, flusher, "thinking", chunk.Delta)
		case hippo.StreamChunkToolCall:
			if chunk.ToolCall != nil {
				payload, _ := json.Marshal(map[string]any{
					"id":   chunk.ToolCall.ID,
					"name": chunk.ToolCall.Name,
					"args": string(chunk.ToolCall.Arguments),
				})
				writeSSE(w, flusher, "tool_call", string(payload))
				record.ToolCalls++
			}
		case hippo.StreamChunkToolResult:
			if chunk.ToolResult != nil {
				payload, _ := json.Marshal(map[string]any{
					"call_id":  chunk.ToolCallID,
					"content":  chunk.ToolResult.Content,
					"is_error": chunk.ToolResult.IsError,
				})
				writeSSE(w, flusher, "tool_result", string(payload))
			}
		case hippo.StreamChunkUsage:
			record.Provider = chunk.Provider
			record.Model = chunk.Model
			if chunk.Usage != nil {
				record.Usage = *chunk.Usage
			}
			record.CostUSD = chunk.CostUSD
			record.LatencyMS = time.Since(start).Milliseconds()
			record.Response = fullText

			payload, _ := json.Marshal(map[string]any{
				"provider":      chunk.Provider,
				"model":         chunk.Model,
				"cost_usd":      chunk.CostUSD,
				"input_tokens":  record.Usage.InputTokens,
				"output_tokens": record.Usage.OutputTokens,
				"latency_ms":    record.LatencyMS,
			})
			writeSSE(w, flusher, "usage", string(payload))
		case hippo.StreamChunkError:
			msg := "stream error"
			if chunk.Error != nil {
				msg = chunk.Error.Error()
			}
			writeSSE(w, flusher, "error", msg)
			record.Error = msg
		}
	}

	writeSSE(w, flusher, "done", "")
	if record.Provider == "" && record.Error == "" {
		// Stream closed without a usage chunk (cancelled). Don't
		// record partial turns to keep the spend table honest.
	} else {
		s.state.Record(record)
	}
}

// writeSSE frames one event and flushes the connection.
func writeSSE(w http.ResponseWriter, f http.Flusher, event, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	// Data may contain embedded newlines — SSE requires each one
	// prefixed with "data: " on the wire. strconv.Quote would escape
	// them, but we want raw; walk the bytes.
	for _, line := range splitLines(data) {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = fmt.Fprintln(w)
	f.Flush()
}

func splitLines(s string) []string {
	if s == "" {
		return []string{""}
	}
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// newSessionID returns a random opaque session identifier.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

