package web

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Chat-drawer REST handlers — /api/chats* endpoints that back the
// slide-in conversation history panel. Kept small and JSON-only; the
// drawer UI talks to them with fetch().

func (s *Server) handleChatListGet(w http.ResponseWriter, r *http.Request) {
	if s.chatStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	sessions, err := s.chatStore.List(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []ChatSessionView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleChatGetOne(w http.ResponseWriter, r *http.Request) {
	if s.chatStore == nil {
		http.Error(w, "chat store unavailable", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	rows, err := s.chatStore.GetFull(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Emit a stable JSON shape with lowercase keys — role, content,
	// meta (nullable), created_at — matching what the drawer renders
	// under each bubble on rehydrate.
	if rows == nil {
		rows = []ChatMessageRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": rows})
}

func (s *Server) handleChatCreate(w http.ResponseWriter, r *http.Request) {
	if s.chatStore == nil {
		http.Error(w, "chat store unavailable", http.StatusServiceUnavailable)
		return
	}
	id, err := s.chatStore.Create(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleChatRename(w http.ResponseWriter, r *http.Request) {
	if s.chatStore == nil {
		http.Error(w, "chat store unavailable", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.chatStore.Rename(r.Context(), id, body.Title); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleChatDelete(w http.ResponseWriter, r *http.Request) {
	if s.chatStore == nil {
		http.Error(w, "chat store unavailable", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if err := s.chatStore.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
