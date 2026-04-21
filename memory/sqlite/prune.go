package sqlite

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// PruneConfig controls the auto-prune worker. Profile records are
// never pruned - user-facing profile facts are considered ground
// truth; applications that want to drop them can call Add/Delete
// themselves.
type PruneConfig struct {
	// WorkingMaxAge deletes Working records older than this. Default
	// 7 days; zero disables the rule.
	WorkingMaxAge time.Duration

	// EpisodicMaxAge deletes Episodic records older than this WITH
	// effective importance below EpisodicImportanceCutoff. High-
	// importance episodic records survive indefinitely. Default 90
	// days; zero disables the rule.
	EpisodicMaxAge time.Duration

	// EpisodicImportanceCutoff is the effective-importance threshold
	// used alongside EpisodicMaxAge. Default 0.2.
	EpisodicImportanceCutoff float64
}

// DefaultPruneConfig returns the shipping defaults. Callers that want
// to tweak one knob can start here and mutate.
func DefaultPruneConfig() PruneConfig {
	return PruneConfig{
		WorkingMaxAge:            7 * 24 * time.Hour,
		EpisodicMaxAge:           90 * 24 * time.Hour,
		EpisodicImportanceCutoff: 0.2,
	}
}

// StartAutoPrune launches a background goroutine that applies cfg on
// the given interval. Returns a stop function; stop is called
// automatically on store.Close, so callers that don't hold a
// reference elsewhere can ignore it.
//
// Idempotent per-tick: if the worker's delete already ran within the
// interval, the next tick is a cheap no-op.
func (s *store) StartAutoPrune(ctx context.Context, cfg PruneConfig, interval time.Duration) (func(), error) {
	if interval <= 0 {
		interval = time.Hour
	}
	if cfg.EpisodicImportanceCutoff <= 0 {
		cfg.EpisodicImportanceCutoff = 0.2
	}
	parent, cancel := context.WithCancel(ctx)
	var once sync.Once
	stop := func() { once.Do(cancel) }

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		// Run once on startup so a freshly-opened store cleans up
		// instead of waiting a full interval.
		if err := s.pruneOnce(parent, cfg); err != nil {
			s.logger.Warn("memory/sqlite: initial auto-prune failed", "err", err)
		}
		for {
			select {
			case <-parent.Done():
				return
			case <-t.C:
				if err := s.pruneOnce(parent, cfg); err != nil {
					s.logger.Warn("memory/sqlite: auto-prune failed", "err", err)
				}
			}
		}
	}()

	s.registerWorker(stop)
	return stop, nil
}

// pruneOnce applies cfg once. Separated for testability - tests can
// call this directly without spinning up a ticker.
func (s *store) pruneOnce(ctx context.Context, cfg PruneConfig) error {
	if cfg.WorkingMaxAge > 0 {
		cutoff := time.Now().Add(-cfg.WorkingMaxAge).UnixNano()
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM memories WHERE kind = ? AND timestamp < ?`,
			string(hippo.MemoryWorking), cutoff); err != nil {
			return fmt.Errorf("memory/sqlite: prune working: %w", err)
		}
	}

	if cfg.EpisodicMaxAge > 0 {
		// Effective-importance math happens inline so low-importance
		// old episodic rows vanish while topical ones stay.
		now := time.Now().UnixNano()
		cutoff := time.Now().Add(-cfg.EpisodicMaxAge).UnixNano()
		// Note: two `now` binds for the effective-importance expr,
		// then the kind + cutoff + cutoff-for-importance.
		query := fmt.Sprintf(`
			DELETE FROM memories
			WHERE id IN (
			    SELECT id FROM memories
			    WHERE kind = ? AND timestamp < ? AND %s < ?
			)`, effectiveImportanceExpr)
		if _, err := s.db.ExecContext(ctx, query,
			string(hippo.MemoryEpisodic), cutoff,
			now, now, // two binds for the CASE
			cfg.EpisodicImportanceCutoff); err != nil {
			return fmt.Errorf("memory/sqlite: prune episodic: %w", err)
		}
	}
	return nil
}

// registerWorker records stop fns so Close can run them in reverse
// order at shutdown.
func (s *store) registerWorker(stop func()) {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	s.workers = append(s.workers, stop)
}

