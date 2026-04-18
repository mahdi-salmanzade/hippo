// Package providers is a container for the concrete LLM backends
// (anthropic, openai, gemini, ollama, openrouter) and a small Registry
// helper that lets Router implementations look up providers by name.
//
// The Provider interface itself lives in the root hippo package; concrete
// providers under this directory import it from there.
package providers

import (
	"fmt"
	"sync"

	"github.com/mahdi-salmanzade/hippo"
)

// Registry is a name -> Provider lookup. It is used by Router
// implementations that reference providers by string slug (e.g. from a
// YAML policy file) instead of by value.
//
// A zero Registry is ready to use. Registry is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	items map[string]hippo.Provider
}

// Register adds p to the registry under p.Name(). It returns an error if
// a provider with the same name is already registered.
func (r *Registry) Register(p hippo.Provider) error {
	// TODO: nil-check p; lock and insert.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = make(map[string]hippo.Provider)
	}
	name := p.Name()
	if _, ok := r.items[name]; ok {
		return fmt.Errorf("providers: %q already registered", name)
	}
	r.items[name] = p
	return nil
}

// Get returns the provider registered under name, or nil if absent.
func (r *Registry) Get(name string) hippo.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.items[name]
}

// Names returns the registered provider names in insertion-independent
// (stable-sorted) order.
func (r *Registry) Names() []string {
	// TODO: sort names for deterministic output.
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.items))
	for k := range r.items {
		out = append(out, k)
	}
	return out
}
