// Package sqlite implements hippo.Memory on top of a local SQLite
// database via modernc.org/sqlite (pure Go, CGO-free).
//
// The schema is intentionally small: one records table with columns for
// id, kind, timestamp, content, tags (JSON array), importance, embedding
// (BLOB, optional), and source. An FTS5 virtual table over content
// provides lexical search when embeddings are unavailable.
package sqlite

import (
	"context"
	"time"

	"github.com/mahdi-salmanzade/hippo"

	// Registers the pure-Go "sqlite" driver with database/sql.
	// Imported for its side effect only.
	_ "modernc.org/sqlite"
)

// store is the SQLite-backed hippo.Memory implementation. Unexported
// because users only interact with it through the hippo.Memory returned
// by Open.
type store struct {
	path string
	// TODO: *sql.DB handle; prepared statements; embedding config.
}

// Open returns a hippo.Memory backed by the SQLite database at path. The
// file is created (with schema) if it does not yet exist.
//
// Open does not import modernc.org/sqlite at scaffolding time; the
// dependency is added when the implementation lands.
func Open(path string) (hippo.Memory, error) {
	// TODO: open sql.DB with driver "sqlite", apply schema migrations,
	// prepare statements, return store.
	return &store{path: path}, nil
}

// Add persists a record. See hippo.Memory.
func (s *store) Add(ctx context.Context, rec *hippo.Record) error {
	_ = ctx
	_ = rec
	// TODO: assign ID if empty, INSERT into records, insert into FTS.
	panic("memory/sqlite: Add not implemented")
}

// Recall queries the store. See hippo.Memory.
func (s *store) Recall(ctx context.Context, query string, q hippo.MemoryQuery) ([]hippo.Record, error) {
	_ = ctx
	_ = query
	_ = q
	// TODO: FTS5 MATCH + query filters, optionally rerank via embeddings.
	panic("memory/sqlite: Recall not implemented")
}

// Prune removes records older than before, excluding profile records.
// See hippo.Memory.
func (s *store) Prune(ctx context.Context, before time.Time) error {
	_ = ctx
	_ = before
	// TODO: DELETE WHERE kind != 'profile' AND timestamp < ?.
	panic("memory/sqlite: Prune not implemented")
}

// Close closes the underlying DB handle.
func (s *store) Close() error {
	// TODO: close prepared statements and db.
	return nil
}
