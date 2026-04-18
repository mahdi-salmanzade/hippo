// Package sqlite implements hippo's Memory interface on top of a local
// SQLite database via modernc.org/sqlite (pure Go, CGO-free).
//
// The schema is intentionally small: one records table with columns for
// id, kind, timestamp, content, tags (JSON array), importance, embedding
// (BLOB, optional), and source. An FTS5 virtual table over content
// provides lexical search when embeddings are unavailable.
package sqlite

import (
	"context"
	"time"

	"github.com/mahdi-salmanzade/hippo/memory"
)

// Store is the SQLite-backed Memory implementation.
type Store struct {
	path string
	// TODO: *sql.DB handle; prepared statements; embedding config.
}

// Open returns a Store backed by the SQLite database at path. The file
// is created (with schema) if it does not yet exist.
//
// Open does not import modernc.org/sqlite at scaffolding time; the
// dependency is added when the implementation lands.
func Open(path string) (*Store, error) {
	// TODO: open sql.DB with driver "sqlite", apply schema migrations,
	// prepare statements, return Store.
	return &Store{path: path}, nil
}

// Add persists a record. See memory.Memory.
func (s *Store) Add(ctx context.Context, rec *memory.Record) error {
	_ = ctx
	_ = rec
	// TODO: assign ID if empty, INSERT into records, insert into FTS.
	panic("memory/sqlite: Add not implemented")
}

// Recall queries the store. See memory.Memory.
func (s *Store) Recall(ctx context.Context, query string, scope memory.Scope) ([]memory.Record, error) {
	_ = ctx
	_ = query
	_ = scope
	// TODO: FTS5 MATCH + scope filters, optionally rerank via embeddings.
	panic("memory/sqlite: Recall not implemented")
}

// Prune removes records older than before, excluding profile records.
// See memory.Memory.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	_ = ctx
	_ = before
	// TODO: DELETE WHERE kind != 'profile' AND timestamp < ?.
	panic("memory/sqlite: Prune not implemented")
}

// Close closes the underlying DB handle.
func (s *Store) Close() error {
	// TODO: close prepared statements and db.
	return nil
}
