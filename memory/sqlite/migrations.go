package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// migration represents one schema change. Each entry bumps the
// schema_version table by exactly 1 and is wrapped in a transaction by
// the runner so a partial failure leaves the database untouched.
//
// Adding a new migration: append to migrations; never edit a committed
// entry. Existing installations run any version < registered and the
// store's Open() returns only once the database is fully migrated.
type migration struct {
	version int
	name    string
	sql     string
}

// migrations is the canonical ordered list. Version numbers are
// contiguous starting at 1 - the runner asserts this to catch accidental
// out-of-order inserts during development.
var migrations = []migration{
	{
		version: 1,
		name:    "initial schema",
		sql:     initialSchemaSQL,
	},
	{
		version: 2,
		name:    "add embedding + access bookkeeping columns",
		sql:     embeddingColumnsSQL,
	},
}

// initialSchemaSQL is the Pass 2 schema. New databases run this, and
// existing v0.1.0 databases skip it because their schema_version is
// already 1 (see reconcileV1Legacy for how we detect them).
const initialSchemaSQL = `
CREATE TABLE IF NOT EXISTS memories (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    timestamp  INTEGER NOT NULL,
    content    TEXT NOT NULL,
    importance REAL NOT NULL DEFAULT 0.5,
    metadata   TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_memories_kind_timestamp ON memories(kind, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_memories_importance ON memories(importance DESC);

CREATE TABLE IF NOT EXISTS tags (
    memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    tag       TEXT NOT NULL,
    PRIMARY KEY (memory_id, tag)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(tag);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
`

// embeddingColumnsSQL is the Pass 11 migration. Adds:
//   - embedding (BLOB, nullable) - packed little-endian float32 vectors.
//   - embedding_model (TEXT, nullable) - whose name produced them.
//   - embedded_at (INTEGER, nullable) - unix-nanoseconds timestamp.
//   - last_accessed (INTEGER, nullable) - ditto, for decay.
//   - access_count (INTEGER, default 0) - for the decay boost term.
//
// SQLite's ALTER TABLE ADD COLUMN is safe against a STRICT table as
// long as the new column either has no constraints or defaults to a
// compatible literal. All five additions qualify.
const embeddingColumnsSQL = `
ALTER TABLE memories ADD COLUMN embedding BLOB;
ALTER TABLE memories ADD COLUMN embedding_model TEXT;
ALTER TABLE memories ADD COLUMN embedded_at INTEGER;
ALTER TABLE memories ADD COLUMN last_accessed INTEGER;
ALTER TABLE memories ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_memories_embedding_pending ON memories(id)
    WHERE embedding IS NULL;
CREATE INDEX IF NOT EXISTS idx_memories_importance_timestamp
    ON memories(importance DESC, timestamp DESC);
`

// migrate brings the database to the latest registered migration's
// version. Safe to call on a fresh, partially-migrated, or
// fully-up-to-date database; this is the only public entry point.
//
// Pre-Pass-11 databases (v0.1.0 shape with no schema_version table)
// are detected and reconciled to version 1 before the runner dispatches
// further migrations.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
		    version INTEGER NOT NULL PRIMARY KEY,
		    applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("memory/sqlite: create schema_version: %w", err)
	}

	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		return err
	}

	if current == 0 {
		// Fresh DB or a legacy Pass 2 DB with the memories table but
		// no schema_version. reconcileV1Legacy figures out which.
		current, err = reconcileV1Legacy(ctx, db)
		if err != nil {
			return err
		}
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := runMigration(ctx, db, m); err != nil {
			return fmt.Errorf("memory/sqlite: migration %d %q: %w", m.version, m.name, err)
		}
	}
	return nil
}

// currentSchemaVersion reads the MAX(version) from schema_version.
// Returns 0 on an empty table - either fresh or legacy.
func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("memory/sqlite: read schema_version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// reconcileV1Legacy tells apart "fresh DB" from "Pass 2 DB pre-migration
// framework". If the memories table exists without schema_version
// tracking, it's a legacy v0.1.0 store and we stamp it at version 1
// so the subsequent migrations add only the embedding columns.
func reconcileV1Legacy(ctx context.Context, db *sql.DB) (int, error) {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories'`).Scan(&exists)
	if err != nil {
		return 0, fmt.Errorf("memory/sqlite: detect legacy: %w", err)
	}
	if exists == 0 {
		// Fresh database - nothing to reconcile.
		return 0, nil
	}
	// Legacy db. Run the version-1 DDL anyway (all CREATE IF NOT
	// EXISTS, so harmless) and record the version.
	if _, err := db.ExecContext(ctx, initialSchemaSQL); err != nil {
		return 0, fmt.Errorf("memory/sqlite: re-apply v1: %w", err)
	}
	// If the caller's legacy DB never had FTS5 (pre-Pass-2
	// installations that predate the virtual table), the CREATE above
	// registered it empty. Repopulate from the content table so
	// keyword search works on existing records.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO memories_fts(rowid, content)
		SELECT rowid, content FROM memories
		WHERE rowid NOT IN (SELECT rowid FROM memories_fts)`); err != nil {
		return 0, fmt.Errorf("memory/sqlite: repopulate fts: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (1, strftime('%s','now')*1000000000)`); err != nil {
		return 0, fmt.Errorf("memory/sqlite: stamp v1: %w", err)
	}
	return 1, nil
}

// runMigration wraps one migration in a transaction and records its
// application on success.
func runMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (?, strftime('%s','now')*1000000000)`,
		m.version); err != nil {
		return err
	}
	return tx.Commit()
}
