// Package memory defines the Memory interface and supporting types for
// hippo's persistent, typed memory store.
//
// hippo distinguishes three kinds of memory, modelled after the
// cognitive-science triplet:
//
//   - Working memory: short-lived context for the current task.
//   - Episodic memory: timestamped events the Brain has observed.
//   - Profile memory: long-lived facts about the user or environment.
//
// Backends implement this interface. The canonical backend lives in
// memory/sqlite and uses modernc.org/sqlite (pure Go, CGO-free).
package memory

import (
	"context"
	"time"
)

// Kind classifies a Record's temporal role.
type Kind string

const (
	// Working memory: short-lived per-session context.
	Working Kind = "working"
	// Episodic memory: timestamped events.
	Episodic Kind = "episodic"
	// Profile memory: durable facts about the user/environment.
	Profile Kind = "profile"
)

// Scope narrows a Recall query.
//
// The zero value selects the most-recent records across all kinds, up to
// a backend-chosen default limit.
type Scope struct {
	// Kinds restricts matches to these Kinds. Empty means any.
	Kinds []Kind
	// Tags requires a record to have at least one of these tags. Empty
	// means no tag filter.
	Tags []string
	// Since restricts matches to records on or after this timestamp.
	Since time.Time
	// Limit caps the number of records returned. Zero uses the backend
	// default (typically 10).
	Limit int
	// MinImportance filters out records below this importance score.
	MinImportance float64
}

// Memory is the persistence contract for hippo's memory layer. Backends
// must be safe for concurrent use.
type Memory interface {
	// Add persists a record. If rec.ID is empty, the backend assigns a
	// new one and mutates rec in place (backends may use any scheme,
	// but ULID is recommended).
	Add(ctx context.Context, rec *Record) error
	// Recall returns records matching the scope, ranked by a backend-
	// defined relevance heuristic against query. If the backend supports
	// embeddings, it SHOULD use them; otherwise lexical ranking is
	// acceptable.
	Recall(ctx context.Context, query string, scope Scope) ([]Record, error)
	// Prune deletes records older than before. Profile records are
	// exempt unless the backend is configured otherwise.
	Prune(ctx context.Context, before time.Time) error
	// Close releases resources.
	Close() error
}
