package web

import (
	"io/fs"
	"net/http"
)

// routes wires up every HTTP endpoint the server exposes. Registered
// once at Start; the mux is not mutable afterwards.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleHome)
	mux.HandleFunc("GET /config", s.handleConfigGet)
	mux.HandleFunc("POST /config", s.handleConfigPost)
	mux.HandleFunc("GET /spend", s.handleSpendGet)
	mux.HandleFunc("GET /chat", s.handleChatGet)
	mux.HandleFunc("POST /chat", s.handleChatPost)
	mux.HandleFunc("GET /chat/stream", s.handleChatStream)
	mux.HandleFunc("GET /policy", s.handlePolicyGet)
	mux.HandleFunc("POST /policy", s.handlePolicyPost)
	mux.HandleFunc("GET /api/recent-calls", s.handleRecentCallsFragment)
	mux.HandleFunc("GET /api/providers", s.handleProvidersJSON)
	mux.HandleFunc("GET /api/models/{provider}", s.handleModelsJSON)
	mux.HandleFunc("GET /login", s.handleLoginGet)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		// Reuse the embedded logo when available; 404 otherwise.
		data, err := fs.ReadFile(s.staticFS, "logo.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(data)
	})
	return mux
}
