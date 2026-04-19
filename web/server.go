package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed all:templates all:static
var embedFS embed.FS

// Version is stamped at build time via -ldflags.
var Version = "dev"

// Server is the embedded web UI. Brain and bundle are swapped
// atomically under bundleMu so /config POST can rebuild them without
// tearing down the HTTP listener.
type Server struct {
	cfg       *Config
	state     *State
	logger    *slog.Logger
	templates *template.Template
	staticFS  fs.FS

	bundleMu sync.RWMutex
	bundle   *BrainBundle

	listener net.Listener
	srv      *http.Server
}

// Option configures a Server during New.
type Option func(*Server)

// WithLogger supplies a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// New builds a Server from cfg. It does not start listening; call
// Start to bind the socket and serve.
func New(cfg *Config, opts ...Option) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("web: New: nil config")
	}
	if err := BindGuard(cfg); err != nil {
		return nil, err
	}
	s := &Server{
		cfg:    cfg,
		state:  NewState(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(s)
	}

	tmpls, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s.templates = tmpls

	staticFS, err := fs.Sub(embedFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static sub-fs: %w", err)
	}
	s.staticFS = staticFS

	bundle, err := BuildBrain(cfg, s.logger)
	if err != nil {
		s.logger.Warn("web: initial brain build failed; serving config page only", "err", err)
	} else {
		s.bundle = bundle
	}
	return s, nil
}

// Start binds the listener and serves until ctx is cancelled or
// Shutdown is called. Blocks until the server exits.
func (s *Server) Start(ctx context.Context) error {
	mux := s.routes()

	var handler http.Handler = mux
	if !isLocalhostBind(s.cfg.Server.Addr) || s.cfg.Server.AuthToken != "" {
		handler = AuthMiddleware(s.cfg.Server.AuthToken)(handler)
	}
	handler = s.logMiddleware(handler)

	s.srv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", s.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.cfg.Server.Addr, err)
	}
	s.listener = ln

	s.logger.Info("web: listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	}
}

// Shutdown stops the HTTP server and releases the brain bundle.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		if err := s.srv.Shutdown(ctx); err != nil {
			return err
		}
	}
	s.bundleMu.Lock()
	b := s.bundle
	s.bundle = nil
	s.bundleMu.Unlock()
	if b != nil {
		_ = b.Close()
	}
	return nil
}

// Addr reports the bound listener address. Empty until Start runs.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Bundle returns the current BrainBundle under a read lock.
func (s *Server) Bundle() *BrainBundle {
	s.bundleMu.RLock()
	defer s.bundleMu.RUnlock()
	return s.bundle
}

// ReplaceBundle atomically swaps the current brain with next and closes
// the previous one. Used after /config POST and /policy POST.
func (s *Server) ReplaceBundle(next *BrainBundle) {
	s.bundleMu.Lock()
	prev := s.bundle
	s.bundle = next
	s.bundleMu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
}

// Config returns the server's Config pointer. Mutations are safe only
// before Start; after Start callers should synthesise a new Config
// and ReplaceBundle.
func (s *Server) Config() *Config { return s.cfg }

// Logger exposes the Server's slog logger for handler use.
func (s *Server) Logger() *slog.Logger { return s.logger }

// State exposes the Server's in-process State.
func (s *Server) State() *State { return s.state }

// Templates returns the parsed template set.
func (s *Server) Templates() *template.Template { return s.templates }

// StaticFS returns the embedded static-asset filesystem.
func (s *Server) StaticFS() fs.FS { return s.staticFS }

// logMiddleware records method, path, status and duration at Info.
func (s *Server) logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		s.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.code,
			"ms", time.Since(start).Milliseconds())
	})
}

// statusWriter captures the response code for logging without
// preventing WriteHeader from reaching the underlying writer.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher when the wrapped ResponseWriter does.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// parseTemplates loads every .html file under web/templates into one
// template set. The layout provides a "content" block that each page
// redefines.
func parseTemplates() (*template.Template, error) {
	funcs := template.FuncMap{
		"formatUSD": func(v float64) string { return fmt.Sprintf("$%.6f", v) },
		"formatUSDShort": func(v float64) string {
			if v >= 1 {
				return fmt.Sprintf("$%.2f", v)
			}
			return fmt.Sprintf("$%.6f", v)
		},
		"formatPct": func(num, denom float64) string {
			if denom <= 0 {
				return "0%"
			}
			return fmt.Sprintf("%.1f%%", 100*num/denom)
		},
		"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"humanTime": func(t time.Time) string {
			return t.Local().Format("15:04:05")
		},
		"version": func() string { return Version },
		"add":     func(a, b int) int { return a + b },
		"sub":     func(a, b int) int { return a - b },
		"toFloat": func(i int64) float64 { return float64(i) },
	}

	t := template.New("").Funcs(funcs)
	entries, err := fs.Sub(embedFS, "templates")
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(entries, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		data, err := fs.ReadFile(entries, path)
		if err != nil {
			return err
		}
		if _, err := t.New(path).Parse(string(data)); err != nil {
			return fmt.Errorf("template %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	return t, nil
}
