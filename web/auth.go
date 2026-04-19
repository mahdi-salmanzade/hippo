package web

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// BindGuard validates that the Config's Server.Addr can be served
// without auth (localhost-only) or, if not, that an auth token has
// been supplied. Returning an error from here is the server's only
// acceptable outcome when the binding would expose an unauthenticated
// network listener.
func BindGuard(cfg *Config) error {
	if cfg == nil {
		return errors.New("web: BindGuard: nil config")
	}
	if isLocalhostBind(cfg.Server.Addr) {
		return nil
	}
	if cfg.Server.AuthToken == "" {
		return errors.New("web: auth token required for non-localhost bind (set server.auth_token or use --auth-token)")
	}
	return nil
}

// isLocalhostBind reports whether addr binds to a local-only address.
// Accepts "127.0.0.1:7844", "[::1]:7844", and "localhost:7844" with
// optional ":port" trailing; ":7844" alone binds to every interface
// and is treated as non-local.
func isLocalhostBind(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		// Handle bracketed IPv6 addresses like "[::1]:7844".
		if strings.HasPrefix(addr, "[") {
			if j := strings.Index(addr, "]"); j >= 0 {
				host = addr[1:j]
			}
		} else {
			host = addr[:i]
		}
	}
	switch {
	case host == "":
		return false
	case host == "localhost":
		return true
	case host == "::1":
		return true
	case strings.HasPrefix(host, "127."):
		return true
	}
	return false
}

// AuthMiddleware wraps h with a token-bearer check. Static assets and
// /favicon.ico bypass the check so the login page can render. Request
// authentication accepts the token via Authorization: Bearer <token>
// or a "hippo_auth" cookie. Comparison uses subtle.ConstantTimeCompare
// to avoid timing-channel leaks.
func AuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if bypassAuth(r.URL.Path) {
				h.ServeHTTP(w, r)
				return
			}
			if tokenMatches(r, token) {
				h.ServeHTTP(w, r)
				return
			}
			// For HTML navigations redirect to /login; for fragment
			// or JSON endpoints return 401 so the client can surface
			// the error.
			if wantsHTML(r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

func bypassAuth(path string) bool {
	switch path {
	case "/login", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(path, "/static/")
}

func tokenMatches(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		supplied := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1 {
			return true
		}
	}
	if c, err := r.Cookie("hippo_auth"); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return true
	}
	return strings.Contains(accept, "text/html")
}
