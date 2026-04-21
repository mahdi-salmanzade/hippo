package web

import (
	_ "embed"
	"net/http"
	"os"

	yamlrouter "github.com/mahdi-salmanzade/hippo/router/yaml"
)

//go:embed default_policy_for_ui.yaml
var defaultPolicyForUI []byte

// policyPageData shapes policy.html's template context.
type policyPageData struct {
	YAML  string
	Path  string
	Error string
}

func (s *Server) handlePolicyGet(w http.ResponseWriter, r *http.Request) {
	var body []byte
	path, _ := ExpandPath(s.cfg.PolicyPath)
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		body = defaultPolicyForUI
	}
	flash, flashErr := readFlash(w, r)
	s.render(w, "policy.html", pageData{
		Title:    "Policy",
		Active:   "policy",
		Flash:    flash,
		FlashErr: flashErr,
		Data: policyPageData{
			YAML: string(body),
			Path: s.cfg.PolicyPath,
		},
	})
}

// handlePolicyPost validates the submitted YAML (via yamlrouter.LoadBytes)
// and, on success, writes it to disk and hot-swaps the Brain's router.
func (s *Server) handlePolicyPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := r.FormValue("policy_yaml")
	path := r.FormValue("policy_path")
	if path == "" {
		path = s.cfg.PolicyPath
	}
	if path == "" {
		path = "~/.hippo/policy.yaml"
	}

	// Validate first - yamlrouter.LoadBytes fails on malformed YAML or
	// unknown privacy tiers before we touch disk or the live router.
	if _, err := yamlrouter.LoadBytes([]byte(body)); err != nil {
		writeFlash(w, "", "policy invalid: "+err.Error())
		http.Redirect(w, r, "/policy", http.StatusSeeOther)
		return
	}

	expanded, err := ExpandPath(path)
	if err != nil {
		writeFlash(w, "", "bad path: "+err.Error())
		http.Redirect(w, r, "/policy", http.StatusSeeOther)
		return
	}
	if err := os.WriteFile(expanded, []byte(body), 0o600); err != nil {
		writeFlash(w, "", "write failed: "+err.Error())
		http.Redirect(w, r, "/policy", http.StatusSeeOther)
		return
	}

	// Remember the path in the config so subsequent runs pick it up.
	if s.cfg.PolicyPath != path {
		s.cfg.PolicyPath = path
		_ = s.cfg.Save()
	}

	// Hot-swap: rebuild the brain with the new policy in place.
	next, err := BuildBrain(s.cfg, s.logger, WithExtraTools(s.builtinTools...))
	if err != nil {
		writeFlash(w, "", "policy saved, rebuild failed: "+err.Error())
		http.Redirect(w, r, "/policy", http.StatusSeeOther)
		return
	}
	s.ReplaceBundle(next)
	writeFlash(w, "Policy saved and router reloaded.", "")
	http.Redirect(w, r, "/policy", http.StatusSeeOther)
}
