package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mahdi-salmanzade/hippo"
)

// ChatStore persists chat transcripts across server restarts so users
// can reopen past conversations from the drawer. Sibling to
// memory/sqlite but intentionally separate — chats and memories have
// different retention models (memories are pruned by importance; chats
// are human-curated).
//
// Storage is a single SQLite file, default ~/.hippo/chats.db. The
// schema has two tables: chat_sessions (one row per conversation) and
// chat_messages (one row per turn, FK to the session). Messages are
// appended as turns complete; the session's updated_at is bumped so
// the drawer's recency sort stays accurate without a subquery.
//
// Thread-safety: sql.DB is safe for concurrent use. All methods take
// ctx so callers can cancel; none block long enough to need batching.
type ChatStore struct {
	db *sql.DB
}

// ChatSessionView is the drawer-facing shape: id, title, and the two
// timestamps the list UI sorts by.
type ChatSessionView struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	MessageCount int      `json:"message_count"`
	// Preview is the first ~80 chars of the first user turn — cheap
	// sub-line snippet under the title in the drawer list.
	Preview string `json:"preview,omitempty"`
}

// NewChatStore opens or creates the SQLite file at path and ensures
// the schema is present. Use ":memory:" for an ephemeral store in
// tests.
func NewChatStore(path string) (*ChatStore, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return nil, fmt.Errorf("web: chat store path: %w", err)
	}
	if expanded == "" {
		return nil, errors.New("web: chat store path is empty")
	}
	// PRAGMA busy_timeout keeps concurrent writes from failing fast
	// with SQLITE_BUSY when two handlers race (e.g. append vs. list).
	dsn := expanded + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		dsn = ":memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("web: open chat store: %w", err)
	}
	// Single writer connection is idiomatic with SQLite; the driver
	// queues concurrent transactions under the hood. 1 connection also
	// makes :memory: tests share state across calls.
	db.SetMaxOpenConns(1)
	s := &ChatStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *ChatStore) migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS chat_sessions (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated ON chat_sessions(updated_at DESC);

	CREATE TABLE IF NOT EXISTS chat_messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		FOREIGN KEY(session_id) REFERENCES chat_sessions(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id);
	`
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("web: chat store migrate: %w", err)
	}
	return nil
}

// Close releases the underlying database handle. Safe on nil.
func (s *ChatStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Create starts a new empty session and returns its id. Title is left
// blank — the first user turn's text becomes the auto-title in Append.
func (s *ChatStore) Create(ctx context.Context) (string, error) {
	id, err := newChatID()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO chat_sessions (id, title, created_at, updated_at) VALUES (?, '', ?, ?)`,
		id, now, now)
	if err != nil {
		return "", fmt.Errorf("web: chat store create: %w", err)
	}
	return id, nil
}

// Append stores one turn. When role=="user" and the session has no
// title yet, the content is truncated to 60 chars and saved as the
// title — a cheap heuristic that matches what users expect without a
// second LLM round-trip.
func (s *ChatStore) Append(ctx context.Context, sessionID, role, content string) error {
	if sessionID == "" {
		return errors.New("web: chat store append: empty session id")
	}
	if role != "user" && role != "assistant" && role != "system" {
		return fmt.Errorf("web: chat store append: invalid role %q", role)
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Enforce that the session exists, otherwise FK insert would fail
	// with a cryptic error. Fetch the current title so we know whether
	// to auto-assign from this user turn.
	var currentTitle string
	err = tx.QueryRowContext(ctx,
		`SELECT title FROM chat_sessions WHERE id = ?`, sessionID).Scan(&currentTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("web: chat store append: session %s not found", sessionID)
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat_messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		sessionID, role, content, now); err != nil {
		return fmt.Errorf("web: chat store append insert: %w", err)
	}

	// Auto-title from first user turn when blank. Keep the original
	// content on the message row; this is just the drawer label.
	if currentTitle == "" && role == "user" {
		title := titleFromUserTurn(content)
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_sessions SET title = ?, updated_at = ? WHERE id = ?`,
			title, now, sessionID); err != nil {
			return fmt.Errorf("web: chat store append update title: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat_sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
			return fmt.Errorf("web: chat store append update ts: %w", err)
		}
	}
	return tx.Commit()
}

// Get returns the full transcript for a session in chronological
// order. Returned as []hippo.Message so callers can feed it straight
// into hippo.Call.Messages.
func (s *ChatStore) Get(ctx context.Context, sessionID string) ([]hippo.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content FROM chat_messages WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("web: chat store get: %w", err)
	}
	defer rows.Close()
	var out []hippo.Message
	for rows.Next() {
		var m hippo.Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// List returns sessions ordered newest-first, capped at limit. Each
// row includes the message count and a one-line preview from the
// first user turn (for the drawer's sub-line).
func (s *ChatStore) List(ctx context.Context, limit int) ([]ChatSessionView, error) {
	if limit <= 0 {
		limit = 50
	}
	// Single query with a correlated subquery for the preview — 50
	// rows is well within SQLite's sweet spot; no need to paginate.
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title, s.created_at, s.updated_at,
		       (SELECT COUNT(*) FROM chat_messages WHERE session_id = s.id) AS cnt,
		       COALESCE(
		         (SELECT content FROM chat_messages
		          WHERE session_id = s.id AND role = 'user'
		          ORDER BY id ASC LIMIT 1),
		         ''
		       ) AS preview
		FROM chat_sessions s
		ORDER BY s.updated_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("web: chat store list: %w", err)
	}
	defer rows.Close()
	var out []ChatSessionView
	for rows.Next() {
		var v ChatSessionView
		var created, updated int64
		if err := rows.Scan(&v.ID, &v.Title, &created, &updated, &v.MessageCount, &v.Preview); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(created, 0)
		v.UpdatedAt = time.Unix(updated, 0)
		// Truncate preview so the drawer list doesn't balloon on a
		// paste-heavy first turn.
		v.Preview = truncatePreview(v.Preview)
		out = append(out, v)
	}
	return out, rows.Err()
}

// Rename sets the title for a session. Empty title clears it (and
// next Append will auto-assign from the first user turn again).
func (s *ChatStore) Rename(ctx context.Context, sessionID, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET title = ? WHERE id = ?`, title, sessionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("web: chat store rename: session %s not found", sessionID)
	}
	return nil
}

// Delete removes a session and its messages (cascade).
func (s *ChatStore) Delete(ctx context.Context, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id = ?`, sessionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("web: chat store delete: session %s not found", sessionID)
	}
	return nil
}

// titleFromUserTurn builds a drawer-display-friendly title from the
// user's first turn. Single-line, trimmed, capped to 60 characters.
func titleFromUserTurn(content string) string {
	t := strings.TrimSpace(content)
	// Single line — newlines are visual noise in a drawer list.
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	const max = 60
	if len(t) > max {
		t = strings.TrimRight(t[:max], " ") + "…"
	}
	if t == "" {
		return "(untitled)"
	}
	return t
}

// newChatID generates an opaque session id. Shorter than the /chat
// stream session id (16 hex bytes would bloat the drawer URLs); 12
// hex bytes gives 48 bits of entropy, plenty for single-user.
func newChatID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
