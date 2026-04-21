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
	"strings"
	"sync"
	"sync/atomic"
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
	db       *sql.DB
	logger   *slog.Logger
	embedder hippo.Embedder

	// workers holds the stop functions of any background goroutines
	// (backfill, auto-prune) started via StartBackfill / StartAutoPrune.
	// Close stops them in reverse order before closing the DB.
	workersMu sync.Mutex
	workers   []func()

	// Backfill status mirror - written by the backfill worker, read
	// by BackfillStatus. The atomic flag lets the web UI poll without
	// contending with the worker; the remaining fields use the mutex.
	lastBackfillRunning atomic.Bool
	backfillStatusMu    sync.Mutex
	lastBackfillError   string
	lastBackfillRunAt   time.Time
}

// Option configures a store during construction.
type Option func(*config)

type config struct {
	busyTimeout time.Duration
	maxOpenConn int
	logger      *slog.Logger
	embedder    hippo.Embedder
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

// WithEmbedder attaches an Embedder so Recall can score candidates on
// vector similarity. Without one the store operates keyword-only and
// MemoryQuery.Semantic is silently ignored.
//
// Safe to pass a nil Embedder (the option becomes a no-op); callers
// that probe an environment for an embedder can keep the code simple.
func WithEmbedder(e hippo.Embedder) Option {
	return func(c *config) { c.embedder = e }
}

// Open creates or opens a SQLite-backed memory store at the given path.
//
// Pass ":memory:" for an in-memory store - useful for tests. Note that
// with the pure-Go modernc.org/sqlite driver, ":memory:" gives each
// database/sql connection its own independent in-memory database; a
// write on connection A is invisible to a read on connection B. Open
// therefore forces MaxOpenConns to 1 when path is ":memory:", and
// logs a warning if the caller supplied WithMaxOpenConns(>1). Tests
// that exercise concurrent access should use a temp file instead.
//
// WAL mode is enabled on file-backed stores via a DSN PRAGMA; the
// schema is created idempotently on every Open.
func Open(path string, opts ...Option) (hippo.Memory, error) {
	cfg := config{
		busyTimeout: defaultBusyTimeout,
		maxOpenConn: defaultMaxOpen,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(&cfg)
	}

	// Guard against the per-connection-isolated :memory: footgun.
	// Keep the log quiet when the user never set WithMaxOpenConns.
	if path == ":memory:" && cfg.maxOpenConn != 1 {
		if cfg.maxOpenConn > 1 {
			cfg.logger.Warn("memory/sqlite: MaxOpenConns>1 ignored for :memory: store; forcing 1",
				"requested", cfg.maxOpenConn,
			)
		}
		cfg.maxOpenConn = 1
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

	if err := migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory/sqlite: schema: %w", err)
	}

	return &store{
		db:       db,
		logger:   cfg.logger,
		embedder: cfg.embedder,
	}, nil
}

// Embedder returns the embedder passed to Open, or nil when the store
// is keyword-only. Internal callers (Brain.New's backfill wiring)
// inspect this to decide whether to start the backfill worker.
func (s *store) Embedder() hippo.Embedder { return s.embedder }

// DB exposes the underlying *sql.DB for operations that need to issue
// custom SQL (the backfill worker, the web UI's stats query).
// Unexported consumers within the hippo tree; external callers should
// stick to the hippo.Memory interface.
func (s *store) DB() *sql.DB { return s.db }

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
//
// TODO(pass3): persist rec.Source into the metadata JSON column
// alongside routing metadata (provider, model, cost, latency).
// TODO(pass4): persist rec.Embedding to a dedicated BLOB column and
// add an ANN index for semantic retrieval.
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

	var (
		embBlob   any
		embModel  any
		embedded  any
	)
	if len(rec.Embedding) > 0 {
		embBlob = encodeEmbedding(rec.Embedding)
		if s.embedder != nil {
			embModel = s.embedder.Name()
		}
		embedded = time.Now().UnixNano()
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories (id, kind, timestamp, content, importance, metadata, created_at,
		                     embedding, embedding_model, embedded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID,
		string(rec.Kind),
		rec.Timestamp.UnixNano(),
		rec.Content,
		rec.Importance,
		"{}",
		time.Now().UnixNano(),
		embBlob,
		embModel,
		embedded,
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

// defaultRecallLimit is used when hippo.MemoryQuery.Limit is zero.
const defaultRecallLimit = 10

// Recall returns records matching q. The retrieval mode is chosen
// from the interaction of (query string, q.Semantic, embedder wired
// up on the store):
//
//   - query + Semantic + embedder  → hybrid (keyword + vector blend)
//   - Semantic alone + embedder    → semantic-only on query text
//   - query + (no Semantic or no embedder) → keyword-only (FTS5 bm25)
//   - empty query                  → recency
//
// Filters (Kinds, Tags, Since/Until, MinImportance) compose with AND
// in every mode. MinImportance runs against effective (decayed)
// importance rather than the raw field so old records fade
// automatically.
func (s *store) Recall(ctx context.Context, query string, q hippo.MemoryQuery) ([]hippo.Record, error) {
	trimmed := strings.TrimSpace(query)
	hasEmbedder := s.embedder != nil
	wantSemantic := q.Semantic && hasEmbedder

	var (
		records []hippo.Record
		err     error
	)
	switch {
	case wantSemantic && trimmed != "":
		records, err = s.recallHybrid(ctx, trimmed, q)
	case wantSemantic:
		// Semantic requested without a text to embed - fall back to
		// recency ordered by decayed importance. Still honours filters.
		records, err = s.recallRecency(ctx, q)
	case trimmed != "":
		records, err = s.recallKeyword(ctx, trimmed, q, effectiveLimit(q))
	default:
		records, err = s.recallRecency(ctx, q)
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadTags(ctx, records); err != nil {
		return nil, err
	}
	if len(records) > 0 {
		s.markAccessed(ctx, recordIDs(records))
	}
	return records, nil
}

// effectiveLimit returns the requested limit or the default.
func effectiveLimit(q hippo.MemoryQuery) int {
	if q.Limit > 0 {
		return q.Limit
	}
	return defaultRecallLimit
}

// recordIDs extracts IDs for follow-up queries (tag loading, last_accessed
// bookkeeping). Linear scan; the slice is always small.
func recordIDs(records []hippo.Record) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID
	}
	return out
}

// loadTags populates the Tags field of each record by issuing a single
// follow-up query across all record IDs. For the typical limit=10
// case this is one extra roundtrip; worth it to keep Recall's SELECT
// simple.
func (s *store) loadTags(ctx context.Context, records []hippo.Record) error {
	if len(records) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("SELECT memory_id, tag FROM tags WHERE memory_id IN (")
	args := make([]any, 0, len(records))
	byID := make(map[string]int, len(records))
	for i, r := range records {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('?')
		args = append(args, r.ID)
		byID[r.ID] = i
	}
	sb.WriteByte(')')

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("memory/sqlite: load tags: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return fmt.Errorf("memory/sqlite: load tags scan: %w", err)
		}
		if idx, ok := byID[id]; ok {
			records[idx].Tags = append(records[idx].Tags, tag)
		}
	}
	return rows.Err()
}

// buildFTSQuery turns user-provided search text into an FTS5 query
// string. Each whitespace-separated token is wrapped as a phrase
// (double quotes escaped by doubling per FTS5 quoting rules) and the
// phrases are OR-joined so multi-word queries recall any matching
// token rather than requiring every one - bm25() still ranks the
// overlapping matches above partial ones, so "the best hit wins"
// holds. The quoting also sanitises FTS5 operators (NOT, NEAR, column
// filters) so user input can't reshape the query.
//
// Pass 11 change from Pass 2: Pass 2 used implicit-AND (" "). With
// semantic recall now carrying the "must match exactly" workload,
// keyword's role shifts to "find anything relevant"; OR-semantics
// matches that posture and matches what most search UIs do by
// default.
func buildFTSQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " OR ")
}

// Prune deletes working-memory records older than before. See
// hippo.Memory.
//
// Only rows with kind='working' are removed. Episodic and Profile
// records are the ground-truth layer and are never auto-deleted -
// Prune is intentionally conservative. Applications that want to
// age-out episodic or profile data should issue that DELETE
// themselves.
//
// Tag rows and FTS5 index entries are cleaned up automatically via
// the ON DELETE CASCADE on tags.memory_id and the memories_ad
// trigger on the memories table.
func (s *store) Prune(ctx context.Context, before time.Time) error {
	if before.IsZero() {
		return errors.New("memory/sqlite: Prune: before is zero")
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM memories WHERE kind = ? AND timestamp < ?`,
		string(hippo.MemoryWorking),
		before.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("memory/sqlite: prune: %w", err)
	}
	return nil
}

// DeleteByID removes one record and its tag rows. The FTS5 trigger
// cleans up the search-index entry automatically, and the tag table's
// ON DELETE CASCADE handles the join table. Missing IDs return nil -
// the caller's view is "the row isn't here" either way.
func (s *store) DeleteByID(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("memory/sqlite: DeleteByID: empty id")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("memory/sqlite: delete %s: %w", id, err)
	}
	return nil
}

// Close stops any registered background workers (backfill, auto-prune)
// in reverse registration order and then closes the underlying *sql.DB.
// Safe to call more than once.
func (s *store) Close() error {
	if s == nil {
		return nil
	}
	s.workersMu.Lock()
	workers := s.workers
	s.workers = nil
	s.workersMu.Unlock()
	for i := len(workers) - 1; i >= 0; i-- {
		workers[i]()
	}
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}
