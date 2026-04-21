package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/mahdi-salmanzade/hippo/budget"
)

// pageData is the common envelope passed to every full-page render.
// Page-specific data rides under Data.
type pageData struct {
	Title    string
	Active   string
	Flash    string
	FlashErr string
	Version  string
	Data     any
	Warnings []string
}

// render executes the named template wrapped in layout.html.
func (s *Server) render(w http.ResponseWriter, name string, d pageData) {
	if d.Version == "" {
		d.Version = Version
	}
	if d.Warnings == nil {
		d.Warnings = s.bundleWarnings()
	}
	var body bytes.Buffer
	if err := s.templates.ExecuteTemplate(&body, name, d); err != nil {
		s.logger.Error("template execute", "name", name, "err", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.Data = template.HTML(body.String())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "layout.html", d); err != nil {
		s.logger.Error("layout execute", "err", err)
	}
}

// renderFragment renders an htmx fragment (no layout wrap).
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("fragment execute", "name", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) bundleWarnings() []string {
	b := s.Bundle()
	if b == nil {
		return nil
	}
	return b.Warnings
}

// handleHome redirects to /chat when providers are configured, else to
// /config so the first-run user lands on the setup flow.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b := s.Bundle()
	if b != nil && b.Brain != nil {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/config", http.StatusFound)
}

// configPageData is what config.html expects.
type configPageData struct {
	Providers    []configProviderView
	ActiveCount  int
	DefaultRoute string // "provider:model" of the first enabled provider with a default; "" otherwise
	Budget       BudgetConfig
	Memory       MemoryConfig
	Policy       string
	MCPServers   []MCPServerView
}

// MCPServerView is the template-facing shape of one MCP server row.
// Command is rendered as a space-joined string; the form handler
// re-splits on whitespace when saving.
type MCPServerView struct {
	Index     int
	Name      string
	Transport string
	Command   string
	URL       string
	Prefix    string
	Enabled   bool
}

type configProviderView struct {
	Name         string
	DisplayName  string
	APIKeySet    bool
	BaseURL      string
	DefaultModel string
	Enabled      bool
	Models       []string
	NeedsBaseURL bool
	NeedsAPIKey  bool
}

func providerDisplayName(name string) string {
	switch name {
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "ollama":
		return "Ollama (local)"
	default:
		return name
	}
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	pd := s.buildConfigView()
	flash, flashErr := readFlash(w, r)
	s.render(w, "config.html", pageData{
		Title:    "Config",
		Active:   "config",
		Flash:    flash,
		FlashErr: flashErr,
		Data:     pd,
	})
}

func (s *Server) buildConfigView() configPageData {
	cfg := s.cfg
	order := []string{"anthropic", "openai", "ollama"}
	views := make([]configProviderView, 0, len(order))
	for _, name := range order {
		pc, ok := cfg.Providers[name]
		if !ok {
			pc = ProviderConfig{}
		}
		views = append(views, configProviderView{
			Name:         name,
			DisplayName:  providerDisplayName(name),
			APIKeySet:    pc.APIKey != "",
			BaseURL:      pc.BaseURL,
			DefaultModel: pc.DefaultModel,
			Enabled:      pc.Enabled,
			Models:       modelIDsFor(name),
			NeedsBaseURL: name == "ollama",
			NeedsAPIKey:  name != "ollama",
		})
	}
	mcpViews := make([]MCPServerView, len(cfg.MCP.Servers))
	for i, s := range cfg.MCP.Servers {
		mcpViews[i] = MCPServerView{
			Index:     i,
			Name:      s.Name,
			Transport: s.Transport,
			Command:   strings.Join(s.Command, " "),
			URL:       s.URL,
			Prefix:    s.Prefix,
			Enabled:   s.Enabled,
		}
	}

	active := 0
	defaultRoute := ""
	for _, v := range views {
		if v.Enabled {
			active++
			if defaultRoute == "" && v.DefaultModel != "" {
				defaultRoute = v.Name + ":" + v.DefaultModel
			}
		}
	}

	return configPageData{
		Providers:    views,
		ActiveCount:  active,
		DefaultRoute: defaultRoute,
		Budget:       cfg.Budget,
		Memory:       cfg.Memory,
		Policy:       cfg.PolicyPath,
		MCPServers:   mcpViews,
	}
}

// modelIDsFor pulls known model ids for a provider from the embedded
// pricing table. Returns nil for unknown providers.
func modelIDsFor(provider string) []string {
	ids := budget.DefaultPricing().Models(provider)
	sortStrings(ids)
	return ids
}

// sortStrings sorts in place (tiny-dep avoidance).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func (s *Server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}

	updated := *s.cfg
	if updated.Providers == nil {
		updated.Providers = map[string]ProviderConfig{}
	}
	// copy map so we don't mutate the live config if save fails
	next := make(map[string]ProviderConfig, len(updated.Providers))
	for k, v := range updated.Providers {
		next[k] = v
	}

	for _, name := range []string{"anthropic", "openai", "ollama"} {
		existing := next[name]
		pc := ProviderConfig{
			APIKey:       existing.APIKey,
			BaseURL:      existing.BaseURL,
			DefaultModel: existing.DefaultModel,
			Enabled:      existing.Enabled,
		}
		if v := r.FormValue(name + "_api_key"); v != "" {
			if v == "__clear__" {
				pc.APIKey = ""
			} else {
				pc.APIKey = v
			}
		}
		if v := r.FormValue(name + "_base_url"); v != "" {
			pc.BaseURL = v
		}
		if v := r.FormValue(name + "_default_model"); v != "" {
			pc.DefaultModel = v
		}
		pc.Enabled = r.FormValue(name+"_enabled") == "on"
		next[name] = pc
	}
	updated.Providers = next

	if v := r.FormValue("budget_ceiling_usd"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			updated.Budget.CeilingUSD = parsed
		}
	}
	updated.Memory.Enabled = r.FormValue("memory_enabled") == "on"
	if v := r.FormValue("memory_db_path"); v != "" {
		updated.Memory.DBPath = v
	}

	updated.MCP = parseMCPForm(r)

	if err := updated.Validate(); err != nil {
		writeFlash(w, "", err.Error())
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}

	updated.path = s.cfg.path
	if err := updated.Save(); err != nil {
		writeFlash(w, "", "save failed: "+err.Error())
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}
	*s.cfg = updated

	next2, err := BuildBrain(s.cfg, s.logger)
	if err != nil {
		writeFlash(w, "config saved, but brain rebuild failed: "+err.Error(), "")
		http.Redirect(w, r, "/config", http.StatusSeeOther)
		return
	}
	s.ReplaceBundle(next2)
	writeFlash(w, "Saved. Brain reconstructed.", "")
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

// spendPageData is the shape spend.html renders.
type spendPageData struct {
	Total        float64
	CallCount    int
	ByProvider   []ProviderSpend
	ByTask       []TaskSpend
	ByModel      []ModelSpend
	RecentCalls  []CallRecord
	CeilingUSD   float64
	BudgetSpent  float64
	BudgetRemain float64
	BudgetRatio  float64 // 0..1, clamped, for the ring
}

func (s *Server) handleSpendGet(w http.ResponseWriter, r *http.Request) {
	pd := spendPageData{
		Total:       s.state.TotalSpend(),
		CallCount:   s.state.CallCount(),
		ByProvider:  s.state.SpendByProvider(),
		ByTask:      s.state.SpendByTask(),
		ByModel:     s.state.SpendByModel(),
		RecentCalls: s.state.Recent(50),
		CeilingUSD:  s.cfg.Budget.CeilingUSD,
	}
	if b := s.Bundle(); b != nil && b.Budget != nil {
		pd.BudgetSpent = b.Budget.Spent()
		pd.BudgetRemain = b.Budget.Remaining()
	}
	if pd.CeilingUSD > 0 {
		ratio := pd.BudgetSpent / pd.CeilingUSD
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		pd.BudgetRatio = ratio
	}
	s.render(w, "spend.html", pageData{
		Title:  "Spend",
		Active: "spend",
		Data:   pd,
	})
}

func (s *Server) handleRecentCallsFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, "fragments/recent_calls_table.html", s.state.Recent(50))
}

func (s *Server) handleProvidersJSON(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name         string   `json:"name"`
		Enabled      bool     `json:"enabled"`
		DefaultModel string   `json:"default_model"`
		Models       []string `json:"models"`
	}
	out := []entry{}
	for _, name := range []string{"anthropic", "openai", "ollama"} {
		pc := s.cfg.Providers[name]
		out = append(out, entry{
			Name:         name,
			Enabled:      pc.Enabled,
			DefaultModel: pc.DefaultModel,
			Models:       modelIDsFor(name),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleModelsJSON(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	ids := modelIDsFor(name)
	if ids == nil {
		ids = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"provider": name,
		"models":   ids,
	})
}

// handleLoginGet renders a minimal form — users paste their token
// here when binding off-localhost.
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>hippo login</title>
<form method=post action="/login" style="font-family:sans-serif;max-width:400px;margin:4em auto">
<h2>hippo</h2>
<p>Enter your auth token.</p>
<input type=password name=token style="width:100%;padding:8px;font-size:16px" autofocus>
<button type=submit style="margin-top:1em;padding:8px 16px;font-size:16px">Sign in</button>
</form>`)
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := r.FormValue("token")
	if token != s.cfg.Server.AuthToken {
		http.Redirect(w, r, "/login?err=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "hippo_auth",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   "hippo_auth",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// readFlash fetches and clears the flash cookie.
func readFlash(w http.ResponseWriter, r *http.Request) (ok, errMsg string) {
	if c, err := r.Cookie("hippo_flash"); err == nil {
		ok = c.Value
		http.SetCookie(w, &http.Cookie{Name: "hippo_flash", Value: "", Path: "/", MaxAge: -1})
	}
	if c, err := r.Cookie("hippo_flash_err"); err == nil {
		errMsg = c.Value
		http.SetCookie(w, &http.Cookie{Name: "hippo_flash_err", Value: "", Path: "/", MaxAge: -1})
	}
	return
}

// writeFlash sets the flash cookies used on next render.
func writeFlash(w http.ResponseWriter, ok, errMsg string) {
	if ok != "" {
		http.SetCookie(w, &http.Cookie{Name: "hippo_flash", Value: ok, Path: "/", MaxAge: 10})
	}
	if errMsg != "" {
		http.SetCookie(w, &http.Cookie{Name: "hippo_flash_err", Value: errMsg, Path: "/", MaxAge: 10})
	}
}
