package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// BackfillConfig tunes the embedding backfill worker. Sensible
// defaults mean callers who just want "do the thing" can pass zero
// values.
type BackfillConfig struct {
	// Embedder is required. It should be the same instance attached
	// via WithEmbedder on the store, so records claim the right
	// embedding_model label.
	Embedder hippo.Embedder
	// BatchSize is how many records the worker embeds per batch.
	// Default 32. Keep modest: large batches punish the embedding
	// server when it's CPU-bound.
	BatchSize int
	// Interval is the pause between batches. Default 2s. Gives the
	// embedder server room to breathe on shared hardware.
	Interval time.Duration
	// MaxPerRun caps work per wake-up so a huge backlog can't starve
	// other work. Default 1000.
	MaxPerRun int
}

// BackfillStatus reports worker progress. Fetched from the web UI on
// a poll so users see the backlog drain in real time.
type BackfillStatus struct {
	Total      int64
	Embedded   int64 // records with current-model embeddings
	Pending    int64 // records without current-model embeddings
	Running    bool
	LastError  string
	LastRunAt  time.Time
}

// backfillWorker holds the per-run configuration and routes status
// back to the owning store's mirror fields so BackfillStatus can read
// them without learning about a worker instance.
type backfillWorker struct {
	store *store
	cfg   BackfillConfig
}

// StartBackfill launches a background goroutine that embeds records
// missing an embedding for the current embedder's model name. Returns
// a stop function that is also registered with the store so Close
// tears the worker down automatically.
//
// Concurrency: Add, Recall, Prune, and the backfill worker may all
// run in parallel; each request takes its own transaction, and the
// status getters use atomic reads so status-poll traffic from the web
// UI doesn't contend with the worker.
func (s *store) StartBackfill(ctx context.Context, cfg BackfillConfig) (func(), error) {
	if cfg.Embedder == nil {
		return nil, errors.New("memory/sqlite: StartBackfill: Embedder is required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 32
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.MaxPerRun <= 0 {
		cfg.MaxPerRun = 1000
	}

	parent, cancel := context.WithCancel(ctx)
	var once sync.Once
	stop := func() { once.Do(cancel) }

	w := &backfillWorker{store: s, cfg: cfg}

	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		// Immediate first pass so a restart doesn't wait Interval to
		// begin catching up.
		w.tick(parent)
		for {
			select {
			case <-parent.Done():
				return
			case <-ticker.C:
				w.tick(parent)
			}
		}
	}()

	s.registerWorker(stop)
	return stop, nil
}

// tick runs one wake-up's worth of backfill work — up to MaxPerRun
// records processed in batches of BatchSize.
func (w *backfillWorker) tick(ctx context.Context) {
	w.store.lastBackfillRunning.Store(true)
	defer w.store.lastBackfillRunning.Store(false)

	done := 0
	for done < w.cfg.MaxPerRun {
		if ctx.Err() != nil {
			return
		}
		n, err := w.processBatch(ctx)
		w.store.backfillStatusMu.Lock()
		if err != nil {
			w.store.lastBackfillError = err.Error()
		} else {
			w.store.lastBackfillError = ""
		}
		w.store.lastBackfillRunAt = time.Now()
		w.store.backfillStatusMu.Unlock()
		if err != nil || n == 0 {
			return
		}
		done += n
	}
}

// processBatch fetches one batch of records missing the current-model
// embedding, embeds their content, and writes the vectors back in one
// transaction. Returns the number of records written.
func (w *backfillWorker) processBatch(ctx context.Context) (int, error) {
	model := w.cfg.Embedder.Name()

	rows, err := w.store.db.QueryContext(ctx, `
		SELECT id, content FROM memories
		WHERE embedding IS NULL OR embedding_model IS NULL OR embedding_model != ?
		ORDER BY timestamp DESC
		LIMIT ?`, model, w.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("memory/sqlite: backfill fetch: %w", err)
	}
	defer rows.Close()

	var ids, texts []string
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return 0, fmt.Errorf("memory/sqlite: backfill scan: %w", err)
		}
		ids = append(ids, id)
		texts = append(texts, content)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("memory/sqlite: backfill iter: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	vectors, err := w.cfg.Embedder.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("memory/sqlite: backfill embed: %w", err)
	}
	if len(vectors) != len(ids) {
		return 0, fmt.Errorf("memory/sqlite: backfill returned %d vectors for %d rows",
			len(vectors), len(ids))
	}

	tx, err := w.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("memory/sqlite: backfill tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE memories
		SET embedding = ?, embedding_model = ?, embedded_at = ?
		WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("memory/sqlite: backfill prepare: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	for i, id := range ids {
		blob := encodeEmbedding(vectors[i])
		if _, err := stmt.ExecContext(ctx, blob, model, now, id); err != nil {
			return 0, fmt.Errorf("memory/sqlite: backfill update %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("memory/sqlite: backfill commit: %w", err)
	}
	return len(ids), nil
}

// BackfillStatus computes live counters by querying the DB. The
// running flag is atomic; last-error/last-run come from a short
// mutex section.
func (s *store) BackfillStatus(ctx context.Context) (BackfillStatus, error) {
	var out BackfillStatus
	if s.embedder == nil {
		// No embedder: nothing to backfill; totals are still useful.
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&out.Total); err != nil {
			return out, err
		}
		return out, nil
	}
	model := s.embedder.Name()
	row := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM memories),
		  (SELECT COUNT(*) FROM memories WHERE embedding IS NOT NULL AND embedding_model = ?),
		  (SELECT COUNT(*) FROM memories WHERE embedding IS NULL OR embedding_model IS NULL OR embedding_model != ?)`,
		model, model)
	if err := row.Scan(&out.Total, &out.Embedded, &out.Pending); err != nil {
		return out, fmt.Errorf("memory/sqlite: backfill status: %w", err)
	}

	s.backfillStatusMu.Lock()
	out.Running = s.lastBackfillRunning.Load()
	out.LastError = s.lastBackfillError
	out.LastRunAt = s.lastBackfillRunAt
	s.backfillStatusMu.Unlock()
	return out, nil
}
