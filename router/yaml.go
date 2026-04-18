package router

import (
	"context"

	"github.com/mahdi-salmanzade/hippo"
)

// yamlRouter loads a Policy from a YAML file and reloads it on change.
// It implements hippo.Router.
//
// The YAML schema mirrors the Policy struct one-to-one. See policy.yaml
// at the repo root for a minimal example.
//
// yamlRouter is safe for concurrent use: policy updates are applied
// atomically to an internal *Policy pointer, and Route reads the pointer
// without locking.
type yamlRouter struct {
	path string
	// TODO: atomic.Pointer[Policy]; fsnotify watcher handle (or poll).
}

// NewYAML constructs a hippo.Router that reads its policy from path. The
// file is loaded synchronously once; if it fails to parse, NewYAML
// returns the error.
//
// Once constructed, the router re-loads the file whenever its mtime
// changes. Parse failures during reload are logged but do not replace
// the in-memory Policy.
func NewYAML(path string) (hippo.Router, error) {
	// TODO: read file, unmarshal via gopkg.in/yaml.v3, store atomically,
	// spawn reload goroutine.
	return &yamlRouter{path: path}, nil
}

// Name returns "yaml".
func (y *yamlRouter) Name() string { return "yaml" }

// Route implements hippo.Router.
func (y *yamlRouter) Route(ctx context.Context, c hippo.Call, budget float64) (hippo.Decision, error) {
	_ = ctx
	_ = c
	_ = budget
	// TODO: look up TaskPolicy by c.Task; walk Prefer then Fallback;
	// skip providers whose Privacy tier is weaker than required or
	// whose EstimateCost exceeds budget; return first viable decision.
	panic("router/yaml: Route not implemented")
}
