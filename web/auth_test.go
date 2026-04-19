package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBindGuardAllowsLocalhost(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7844", "[::1]:7844", "localhost:7844", "127.5.5.5:9000"} {
		c := &Config{Server: ServerConfig{Addr: addr}}
		if err := BindGuard(c); err != nil {
			t.Errorf("BindGuard(%q) = %v; want nil", addr, err)
		}
	}
}

func TestBindGuardRequiresTokenForPublicBind(t *testing.T) {
	c := &Config{Server: ServerConfig{Addr: "0.0.0.0:7844"}}
	if err := BindGuard(c); err == nil {
		t.Fatal("BindGuard(0.0.0.0:7844) with no token; want error")
	}
}

func TestBindGuardAllowsPublicBindWithToken(t *testing.T) {
	c := &Config{Server: ServerConfig{Addr: "0.0.0.0:7844", AuthToken: "secret"}}
	if err := BindGuard(c); err != nil {
		t.Fatalf("BindGuard(0.0.0.0:7844,token) = %v; want nil", err)
	}
}

func TestAuthMiddlewareBypassesStatic(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AuthMiddleware("secret")(next)
	req := httptest.NewRequest("GET", "/static/app.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("static got %d; want 200", w.Code)
	}
}

func TestAuthMiddlewareAcceptsBearer(t *testing.T) {
	h := AuthMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("bearer got %d; want 200", w.Code)
	}
}

func TestAuthMiddlewareAcceptsCookie(t *testing.T) {
	h := AuthMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/config", nil)
	req.AddCookie(&http.Cookie{Name: "hippo_auth", Value: "secret"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("cookie got %d; want 200", w.Code)
	}
}

func TestAuthMiddlewareRejectsWrongToken(t *testing.T) {
	h := AuthMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/config", nil)
	req.Header.Set("Authorization", "Bearer nope")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token got %d; want 401", w.Code)
	}
}

func TestAuthMiddlewareHTMLNavigationRedirectsToLogin(t *testing.T) {
	h := AuthMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/config", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("browser got %d; want 302", w.Code)
	}
}
