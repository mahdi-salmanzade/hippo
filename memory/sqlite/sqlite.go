// Package sqlite implements hippo.Memory on top of a local SQLite
// database via modernc.org/sqlite (pure Go, CGO-free).
//
// The schema is intentionally small: one memories table (id, kind,
// timestamp, content, importance, metadata, created_at), one tags
// table (memory_id, tag), and one FTS5 virtual table kept in sync by
// triggers. WAL mode is enabled for concurrent reads during writes.
//
// Pass 2 is keyword-only: no embeddings, no semantic retrieval, no
// temporal expansion. Recall ranks FTS5 matches by bm25(), falls back
// to recency for empty queries, and honours the MemoryQuery filters
// with AND semantics.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/mahdi-salmanzade/hippo"

	// Registers the pure-Go "sqlite" driver with database/sql.
	// Imported for its side effect only.
	_ "modernc.org/sqlite"
)

const (
	defaultBusyTimeout = 5 * time.Second
	defaultMaxOpen     = 1 // SQLite prefers serialised writes
)

// store is the SQLite-backed hippo.Memory implementation. Unexported
// because users only interact with it through the hippo.Memory
// returned by Open.
type store struct {
	db     *sql.DB
	logger *slog.Logger
}

// Option configures a store during construction.
type Option func(*config)

type config struct {
	busyTimeout time.Duration
	maxOpenConn int
	logger      *slog.Logger
}

// WithBusyTimeout overrides the default 5s SQLite busy_timeout PRAGMA.
// This is how long a writer waits if the database is currently locked
// by another connection before returning SQLITE_BUSY.
func WithBusyTimeout(d time.Duration) Option {
	return func(c *config) { c.busyTimeout = d }
}

// WithMaxOpenConns overrides the default 1-connection cap on the
// underlying *sql.DB. SQLite serialises writes regardless of this
// setting, but multiple readers can benefit from more connections when
// WAL mode is active.
func WithMaxOpenConns(n int) Option {
	return func(c *config) { c.maxOpenConn = n }
}

// WithLogger supplies a structured logger for store-internal diagnostics.
// Defaults to a discard logger; store operations are not chatty.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// Open creates or opens a SQLite-backed memory store at the given path.
// Pass ":memory:" for an in-memory store (used by tests). The schema
// is created idempotently on every Open; upgrading an existing file
// is safe as long as the schema hasn't drifted manually.
func Open(path string, opts ...Option) (hippo.Memory, error) {
	cfg := config{
		busyTimeout: defaultBusyTimeout,
		maxOpenConn: defaultMaxOpen,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(&cfg)
	}

	// modernc.org/sqlite registers under the driver name "sqlite".
	// We pass the busy_timeout and pragmas via DSN parameters so they
	// apply to every connection in the pool, not just the one we
	// happen to use for the initial schema setup.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout=%d&_pragma=journal_mode=WAL&_pragma=synchronous=NORMAL&_pragma=foreign_keys=ON",
		path, cfg.busyTimeout.Milliseconds())

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(cfg.maxOpenConn)

	// Probe the connection up front so Open fails fast on bad paths
	// or permission errors rather than deferring the error to the
	// first Add/Recall call.
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory/sqlite: ping: %w", err)
	}

	if err := applySchema(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory/sqlite: schema: %w", err)
	}

	return &store{db: db, logger: cfg.logger}, nil
}

// applySchema runs the create-if-not-exists statements in a single
// transaction. Idempotent: running it against an already-initialised
// database is a no-op.
func applySchema(ctx context.Context, db *sql.DB) error {
	const ddl = `
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
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES (new.rowid, new.content);
END;
`
	_, err := db.ExecContext(ctx, ddl)
	return err
}

// Add persists a record. See hippo.Memory.
//
// Contract:
//   - rec.Content must be non-empty; otherwise an error is returned
//     and nothing is written.
//   - rec.ID defaults to a freshly generated ULID when empty. The ID
//     is assigned back onto the caller's *Record.
//   - rec.Timestamp defaults to time.Now() when zero.
//   - rec.Importance is clamped to [0, 1].
//   - Tag insertion and the memories INSERT happen inside a single
//     transaction, so a partial write cannot leave tags orphaned.
func (s *store) Add(ctx context.Context, rec *hippo.Record) error {
	if rec == nil {
		return errors.New("memory/sqlite: Add: rec is nil")
	}
	if rec.Content == "" {
		return errors.New("memory/sqlite: Add: content is empty")
	}

	if rec.ID == "" {
		id, err := newULID()
		if err != nil {
			return fmt.Errorf("memory/sqlite: generate id: %w", err)
		}
		rec.ID = id
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}
	if rec.Importance < 0 {
		rec.Importance = 0
	} else if rec.Importance > 1 {
		rec.Importance = 1
	}
	if rec.Kind == "" {
		rec.Kind = hippo.MemoryEpisodic
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory/sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories (id, kind, timestamp, content, importance, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID,
		string(rec.Kind),
		rec.Timestamp.UnixNano(),
		rec.Content,
		rec.Importance,
		"{}",
		time.Now().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("memory/sqlite: insert memory: %w", err)
	}

	if len(rec.Tags) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO tags (memory_id, tag) VALUES (?, ?)`)
		if err != nil {
			return fmt.Errorf("memory/sqlite: prepare tag insert: %w", err)
		}
		defer stmt.Close()
		for _, tag := range rec.Tags {
			if tag == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, rec.ID, tag); err != nil {
				return fmt.Errorf("memory/sqlite: insert tag %q: %w", tag, err)
			}
		}
	}

	return tx.Commit()
}

// newULID returns a fresh 26-character Crockford-Base32 ULID.
//
// Layout: 48-bit millisecond timestamp || 80-bit crypto/rand. This is
// the standard ULID spec (github.com/ulid/spec); we hand-roll it to
// avoid pulling a dependency for a ~30-line function.
func newULID() (string, error) {
	const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ts := uint64(time.Now().UnixMilli()) & ((1 << 48) - 1)

	var out [26]byte
	// 10 chars of timestamp (top char uses only 3 bits).
	out[0] = encoding[(ts>>45)&0x1F]
	out[1] = encoding[(ts>>40)&0x1F]
	out[2] = encoding[(ts>>35)&0x1F]
	out[3] = encoding[(ts>>30)&0x1F]
	out[4] = encoding[(ts>>25)&0x1F]
	out[5] = encoding[(ts>>20)&0x1F]
	out[6] = encoding[(ts>>15)&0x1F]
	out[7] = encoding[(ts>>10)&0x1F]
	out[8] = encoding[(ts>>5)&0x1F]
	out[9] = encoding[ts&0x1F]

	// 16 chars of randomness (80 bits). Each output char is 5 bits.
	var r [10]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	out[10] = encoding[(r[0]&0xF8)>>3]
	out[11] = encoding[((r[0]&0x07)<<2)|((r[1]&0xC0)>>6)]
	out[12] = encoding[(r[1]&0x3E)>>1]
	out[13] = encoding[((r[1]&0x01)<<4)|((r[2]&0xF0)>>4)]
	out[14] = encoding[((r[2]&0x0F)<<1)|((r[3]&0x80)>>7)]
	out[15] = encoding[(r[3]&0x7C)>>2]
	out[16] = encoding[((r[3]&0x03)<<3)|((r[4]&0xE0)>>5)]
	out[17] = encoding[r[4]&0x1F]
	out[18] = encoding[(r[5]&0xF8)>>3]
	out[19] = encoding[((r[5]&0x07)<<2)|((r[6]&0xC0)>>6)]
	out[20] = encoding[(r[6]&0x3E)>>1]
	out[21] = encoding[((r[6]&0x01)<<4)|((r[7]&0xF0)>>4)]
	out[22] = encoding[((r[7]&0x0F)<<1)|((r[8]&0x80)>>7)]
	out[23] = encoding[(r[8]&0x7C)>>2]
	out[24] = encoding[((r[8]&0x03)<<3)|((r[9]&0xE0)>>5)]
	out[25] = encoding[r[9]&0x1F]
	return string(out[:]), nil
}

// Recall is a stub until Pass 2.4 lands.
func (s *store) Recall(ctx context.Context, query string, q hippo.MemoryQuery) ([]hippo.Record, error) {
	_ = ctx
	_ = query
	_ = q
	return nil, hippo.ErrNotImplemented
}

// Prune is a stub until Pass 2.5 lands.
func (s *store) Prune(ctx context.Context, before time.Time) error {
	_ = ctx
	_ = before
	return hippo.ErrNotImplemented
}

// Close closes the underlying *sql.DB. Safe to call more than once.
func (s *store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}
