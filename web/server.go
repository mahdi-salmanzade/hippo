package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mahdi-salmanzade/hippo"
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

	// builtinTools are the hippo-native tools the server registers on
	// every Brain build. They hold closures back to s.Bundle() / s.state
	// so they always resolve against the current bundle — important
	// because bundles get swapped on /config POST and /policy POST.
	builtinTools []hippo.Tool

	// chatStore persists chat transcripts so users can reopen past
	// conversations from the drawer. Best-effort: if the store fails
	// to open, the server logs a warning and chat still works in
	// ephemeral mode (no history).
	chatStore *ChatStore

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

	// Built-in tools capture s via closures; they're safe to construct
	// before Bundle is assigned since invocation only happens inside
	// Brain.Stream, by which point s.bundle is populated.
	s.builtinTools = newBuiltinTools(s.state, s.cfg, s.Bundle)

	// Chat persistence. Failure is not fatal — a missing / unwritable
	// ~/.hippo/chats.db is logged and the drawer UI shows an empty
	// state. The in-memory transcript still works during the session.
	chatDBPath := cfg.Chat.DBPath
	if chatDBPath == "" {
		chatDBPath = "~/.hippo/chats.db"
	}
	if cs, err := NewChatStore(chatDBPath); err != nil {
		s.logger.Warn("web: chat store unavailable", "err", err)
	} else {
		s.chatStore = cs
	}

	// Spend persistence. We load recent-calls from JSON before we
	// build the Brain so the budget tracker can be seeded with the
	// loaded spend (see BuildBrain). Failure to read is not fatal —
	// the UI just starts with an empty Recent Calls table.
	spendPath := cfg.Spend.PersistPath
	if spendPath == "" {
		spendPath = "~/.hippo/spend.json"
	}
	if expanded, err := ExpandPath(spendPath); err == nil && expanded != "" {
		if err := s.state.LoadFrom(expanded); err != nil {
			s.logger.Warn("web: spend state load failed", "path", expanded, "err", err)
		}
		s.state.SetPersistPath(expanded)
	}

	bundle, err := BuildBrain(cfg, s.logger, WithExtraTools(s.builtinTools...))
	if err != nil {
		s.logger.Warn("web: initial brain build failed; serving config page only", "err", err)
	} else {
		s.bundle = bundle
		// Seed the budget tracker with loaded spend so the daily-spent
		// number matches the Recent Calls table after a cold start. We
		// don't have a BudgetTracker.Set method; Charge is the only
		// way in, and that's what the stream handler calls on every
		// live turn, so replaying the loaded usage stays consistent.
		if bundle.Budget != nil {
			for _, r := range s.state.Recent(0) {
				if r.Provider == "" {
					continue
				}
				_ = bundle.Budget.Charge(r.Provider, r.Model, r.Usage)
			}
		}
	}
	return s, nil
}

// builtinToolNames reports the names of the server's registered
// built-in tools. Used by the chat page to show an accurate tool count.
func (s *Server) builtinToolNames() []string {
	out := make([]string, 0, len(s.builtinTools))
	for _, t := range s.builtinTools {
		out = append(out, t.Name())
	}
	return out
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
	if s.chatStore != nil {
		_ = s.chatStore.Close()
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
		// formatUSDShort picks a precision that's readable without being
		// visual noise: whole dollars use 2 decimals, sub-dollar spend
		// uses 6 (fractions-of-a-cent need to be legible), and exact
		// zero shows as "$0.00" so the empty state doesn't render
		// "$0.000000" under a 140px ring.
		"formatUSDShort": func(v float64) string {
			switch {
			case v == 0:
				return "$0.00"
			case v >= 1:
				return fmt.Sprintf("$%.2f", v)
			default:
				return fmt.Sprintf("$%.6f", v)
			}
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
		// ringOffset returns the stroke-dashoffset for a ring at the
		// given 0..1 ratio. Circumference is 2π(size-stroke)/2 so the
		// caller passes matching size/stroke as template ints.
		"ringOffset": func(ratio float64, size, stroke int) string {
			r := float64(size-stroke) / 2
			circ := 2 * math.Pi * r
			if ratio < 0 {
				ratio = 0
			}
			if ratio > 1 {
				ratio = 1
			}
			return fmt.Sprintf("%.2f", circ*(1-ratio))
		},
		"ringCirc": func(size, stroke int) string {
			r := float64(size-stroke) / 2
			return fmt.Sprintf("%.2f", 2*math.Pi*r)
		},
		// providerTone maps a provider name to a pill color class.
		"providerTone": func(p string) string {
			switch p {
			case "anthropic":
				return "rust"
			case "openai":
				return "cyan"
			case "ollama":
				return "moss"
			}
			return "neutral"
		},
		// barWidth returns a width percent for a value vs max, clamped
		// so the smallest non-zero bar is still visible.
		"barWidth": func(v, max float64) string {
			if max <= 0 {
				return "0%"
			}
			p := 100 * v / max
			if v > 0 && p < 2 {
				p = 2
			}
			return fmt.Sprintf("%.1f%%", p)
		},
		// sparkPath builds a polyline "d" attribute from a slice of
		// floats, scaled to the given width/height. Returns the d
		// string; caller splits into line vs area as needed.
		"sparkPath": func(values []float64, w, h int) string {
			if len(values) < 2 {
				return ""
			}
			min, max := values[0], values[0]
			for _, v := range values {
				if v < min {
					min = v
				}
				if v > max {
					max = v
				}
			}
			rg := max - min
			if rg == 0 {
				rg = 1
			}
			parts := make([]string, 0, len(values))
			for i, v := range values {
				x := float64(i) * float64(w) / float64(len(values)-1)
				y := float64(h) - ((v-min)/rg)*(float64(h)-4) - 2
				if i == 0 {
					parts = append(parts, fmt.Sprintf("M%.1f %.1f", x, y))
				} else {
					parts = append(parts, fmt.Sprintf("L%.1f %.1f", x, y))
				}
			}
			return strings.Join(parts, " ")
		},
		// trendDelta returns "+NN.N%" or "-NN.N%" for the last-vs-prev
		// segments of a slice. Prev is the mean of all-but-last; last
		// is the final value. Empty slice returns "".
		// cumulativeCost turns a slice of CallRecords (newest first) into
		// a cumulative-cost series (oldest first) for the sparkline.
		"cumulativeCost": func(rs []CallRecord) []float64 {
			if len(rs) == 0 {
				return nil
			}
			out := make([]float64, len(rs))
			var running float64
			for i := range rs {
				// rs is newest-first; walk backwards to build
				// oldest-first cumulative series.
				rec := rs[len(rs)-1-i]
				running += rec.CostUSD
				out[i] = running
			}
			return out
		},
		"trendDelta": func(values []float64) string {
			if len(values) < 2 {
				return ""
			}
			last := values[len(values)-1]
			var prevSum float64
			for _, v := range values[:len(values)-1] {
				prevSum += v
			}
			prev := prevSum / float64(len(values)-1)
			if prev == 0 {
				return ""
			}
			d := 100 * (last - prev) / prev
			if d >= 0 {
				return fmt.Sprintf("+%.1f%%", d)
			}
			return fmt.Sprintf("%.1f%%", d)
		},
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
