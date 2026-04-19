package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// Decay parameters. Half-lives are hours; zero means "no decay"
// (used by the Profile kind so long-term profile facts don't fade).
//
// The multiplier (1 + ln(1+access_count)/10) rewards frequently-recalled
// records without letting the access boost dominate base importance
// for cold records; divisor of 10 keeps the boost under ~1.7× even at
// thousands of hits.
//
// Changing these constants changes ranking for every query, so
// promotions to the public surface should happen behind an Option;
// for v0.2 they're tuned defaults.
const (
	workingHalfLifeHours  = 24.0     // Working memory fades over a day
	episodicHalfLifeHours = 720.0    // Episodic fades over 30 days
	profileHalfLifeHours  = 0.0      // Profile never decays (sentinel)
)

// effectiveImportanceExpr returns a SQL fragment that computes the
// decayed importance. Inlined into every SELECT that ranks by or
// filters on importance.
//
// Two `?` placeholders, both bound to now_ns. The expression uses
// SQLite's pow() because modernc.org/sqlite ships the math module by
// default; if a future driver drops it, switch to exp(-age * ln(2) /
// half_life) which is algebraically identical.
//
// Kind-specific half-lives are encoded as a CASE so one query handles
// all three kinds. Profile records bypass decay entirely.
// access_count is NULL on pre-Pass-11 rows until the first Recall
// bumps it — COALESCE keeps the arithmetic safe either way.
const effectiveImportanceExpr = `
(importance *
  CASE kind
    WHEN 'profile' THEN 1.0
    WHEN 'working' THEN pow(0.5, ((? - timestamp) / 3600000000000.0) / 24.0)
    ELSE              pow(0.5, ((? - timestamp) / 3600000000000.0) / 720.0)
  END *
  (1 + ln(1 + COALESCE(access_count, 0)) / 10.0)
)`

// scoredRecord pairs a Record with a ranking score and, for hybrid
// recall, the unblended keyword/semantic components used to compute
// it. Kept inside this file — external callers don't need it.
type scoredRecord struct {
	rec       hippo.Record
	embedding []float32
	score     float64
	keyword   float64
	semantic  float64
}

// recallKeyword scans memories_fts for candidates, ranked by bm25
// (normalised into a 0..1 score). Pass 2 behavior preserved; extra
// columns (embedding, access_count) come back too so hybrid recall
// can reuse the rows without re-querying.
func (s *store) recallKeyword(ctx context.Context, query string, q hippo.MemoryQuery, limit int) ([]hippo.Record, error) {
	scored, err := s.recallKeywordScored(ctx, query, q, limit)
	if err != nil {
		return nil, err
	}
	out := make([]hippo.Record, len(scored))
	for i, sc := range scored {
		out[i] = sc.rec
	}
	return out, nil
}

// recallKeywordScored is like recallKeyword but keeps the scoredRecord
// around for hybrid re-ranking.
func (s *store) recallKeywordScored(ctx context.Context, query string, q hippo.MemoryQuery, limit int) ([]scoredRecord, error) {
	now := time.Now().UnixNano()
	// The effectiveImportanceExpr references `? - timestamp` twice
	// (once per CASE branch); both bind to `now`. Then the MATCH
	// predicate binds the FTS query.
	args := []any{now, now, buildFTSQuery(query)}

	var sb strings.Builder
	fmt.Fprintf(&sb, `
		SELECT m.id, m.kind, m.timestamp, m.content, m.importance,
		       COALESCE(m.embedding, X''), bm25(memories_fts),
		       %s
		FROM memories m
		JOIN memories_fts fts ON fts.rowid = m.rowid
		WHERE memories_fts MATCH ?`, effectiveImportanceExpr)

	applyCommonFilters(&sb, &args, q)
	sb.WriteString(" ORDER BY bm25(memories_fts) LIMIT ?")
	args = append(args, limit)

	scored, err := s.scanScored(ctx, sb.String(), args, scanModeKeyword)
	if err != nil {
		return nil, err
	}
	return applyMinImportance(scored, q.MinImportance), nil
}

// recallRecency returns records ordered by timestamp DESC, honouring
// filters and the decay-based MinImportance cutoff.
func (s *store) recallRecency(ctx context.Context, q hippo.MemoryQuery) ([]hippo.Record, error) {
	now := time.Now().UnixNano()
	args := []any{now, now}

	var sb strings.Builder
	fmt.Fprintf(&sb, `
		SELECT m.id, m.kind, m.timestamp, m.content, m.importance,
		       COALESCE(m.embedding, X''), 0.0,
		       %s
		FROM memories m
		WHERE 1=1`, effectiveImportanceExpr)

	applyCommonFilters(&sb, &args, q)
	sb.WriteString(" ORDER BY m.timestamp DESC LIMIT ?")
	args = append(args, effectiveLimit(q))

	scored, err := s.scanScored(ctx, sb.String(), args, scanModeRecency)
	if err != nil {
		return nil, err
	}
	scored = applyMinImportance(scored, q.MinImportance)
	out := make([]hippo.Record, len(scored))
	for i, sc := range scored {
		out[i] = sc.rec
	}
	return out, nil
}

// applyMinImportance drops rows whose effective importance (already
// written into rec.Importance by scanScored) is below the cutoff.
// Zero cutoff is a no-op so the Pass-2 behavior of "no filtering" is
// preserved when MinImportance is unset.
func applyMinImportance(rows []scoredRecord, cutoff float64) []scoredRecord {
	if cutoff <= 0 {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		if r.rec.Importance >= cutoff {
			out = append(out, r)
		}
	}
	return out
}

// recallHybrid implements the MemMachine-flavoured keyword + vector
// blend with nucleus temporal expansion.
func (s *store) recallHybrid(ctx context.Context, query string, q hippo.MemoryQuery) ([]hippo.Record, error) {
	weight := q.HybridWeight
	if weight == 0 {
		weight = 0.6 // default bias toward semantic; keyword still wins on exact matches
	} else if weight < 0 {
		weight = 0
	} else if weight > 1 {
		weight = 1
	}

	// 1. Embed the query text.
	vecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		// Embedder failure isn't fatal: degrade to keyword-only so
		// the caller still gets useful results instead of an error.
		s.logger.Warn("memory/sqlite: embed query failed; degrading to keyword",
			"err", err)
		return s.recallKeyword(ctx, query, q, effectiveLimit(q))
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return s.recallKeyword(ctx, query, q, effectiveLimit(q))
	}
	queryVec := vecs[0]

	limit := effectiveLimit(q)

	// 2. Pull a generous candidate set from FTS5. 3× gives room for
	//    re-ranking without exhausting the DB on huge tables.
	keywordCandidates, err := s.recallKeywordScored(ctx, query, q, limit*3)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]*scoredRecord, len(keywordCandidates)*2)
	for i := range keywordCandidates {
		c := &keywordCandidates[i]
		c.semantic = float64(cosineSimilarity(queryVec, c.embedding))
		// keyword is stored as (-bm25) which is higher-is-better: bm25
		// returns negative numbers in SQLite's FTS5 ranking
		// convention. Normalise by clamping to [0, 1].
		c.keyword = clamp01(c.keyword)
		c.score = (1-weight)*c.keyword + weight*c.semantic
		seen[c.rec.ID] = c
	}

	// 3. Scan pure-semantic winners from records outside the keyword
	//    candidate set. Cap the scan at 5000 most recent embedded
	//    records so a table with millions of rows doesn't trigger a
	//    full scan per recall.
	semanticOnly, err := s.scanSemanticOnly(ctx, queryVec, q, 5000)
	if err != nil {
		return nil, err
	}
	for i := range semanticOnly {
		c := &semanticOnly[i]
		if _, dup := seen[c.rec.ID]; dup {
			continue
		}
		c.score = weight * c.semantic
		seen[c.rec.ID] = c
	}

	// 4. Sort by combined score, take top-k.
	top := make([]*scoredRecord, 0, len(seen))
	for _, v := range seen {
		top = append(top, v)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].score > top[j].score })
	if len(top) > limit {
		top = top[:limit]
	}

	// 5. Nucleus temporal expansion around each hit.
	if q.TemporalExpansion > 0 {
		top = s.expandTemporally(ctx, top, q)
	}

	// 6. Flatten to Records in score order.
	out := make([]hippo.Record, len(top))
	for i, sc := range top {
		out[i] = sc.rec
	}
	return out, nil
}

// scanSemanticOnly pulls the most-recent embedded records (ignoring
// the search text) and returns those whose cosine similarity against
// queryVec is above a modest threshold. Uses the idx_memories_embedded_at
// index when available; filters honour the common query shape so
// Kinds/Tags/Since/etc. still apply.
func (s *store) scanSemanticOnly(ctx context.Context, queryVec []float32, q hippo.MemoryQuery, scanLimit int) ([]scoredRecord, error) {
	now := time.Now().UnixNano()
	args := []any{now, now}

	var sb strings.Builder
	fmt.Fprintf(&sb, `
		SELECT m.id, m.kind, m.timestamp, m.content, m.importance,
		       m.embedding, 0.0,
		       %s
		FROM memories m
		WHERE m.embedding IS NOT NULL`, effectiveImportanceExpr)

	applyCommonFilters(&sb, &args, q)
	sb.WriteString(" ORDER BY m.timestamp DESC LIMIT ?")
	args = append(args, scanLimit)

	rows, err := s.scanScored(ctx, sb.String(), args, scanModeSemantic)
	if err != nil {
		return nil, err
	}

	// Score each by cosine and keep the ones above a small threshold —
	// cosine results for unrelated records tend to cluster around ~0.
	out := rows[:0]
	for _, r := range rows {
		sim := cosineSimilarity(queryVec, r.embedding)
		if sim <= 0.2 {
			continue
		}
		r.semantic = float64(sim)
		out = append(out, r)
	}
	return out, nil
}

// expandTemporally pulls records within ±q.TemporalExpansion of each
// current hit's timestamp, at half the hit's score, and merges them
// into the sorted result. Hits already present are untouched. Honors
// the original q.Limit by capping the expanded set at 3× the limit
// so one noisy hit can't swamp the output.
func (s *store) expandTemporally(ctx context.Context, hits []*scoredRecord, q hippo.MemoryQuery) []*scoredRecord {
	if len(hits) == 0 {
		return hits
	}
	limit := effectiveLimit(q)
	maxOut := limit * 3

	seen := make(map[string]bool, len(hits))
	for _, h := range hits {
		seen[h.rec.ID] = true
	}
	expanded := make([]*scoredRecord, len(hits))
	copy(expanded, hits)

	for _, h := range hits {
		if len(expanded) >= maxOut {
			break
		}
		lower := h.rec.Timestamp.Add(-q.TemporalExpansion)
		upper := h.rec.Timestamp.Add(q.TemporalExpansion)
		neighbors, err := s.recallTimeWindow(ctx, lower, upper, q)
		if err != nil {
			s.logger.Warn("memory/sqlite: temporal expansion neighbor fetch failed",
				"id", h.rec.ID, "err", err)
			continue
		}
		const maxPerHit = 5 // cap so one hit doesn't flood results
		added := 0
		for _, n := range neighbors {
			if seen[n.rec.ID] {
				continue
			}
			if added >= maxPerHit {
				break
			}
			seen[n.rec.ID] = true
			n.score = h.score * 0.5
			expanded = append(expanded, n)
			added++
			if len(expanded) >= maxOut {
				break
			}
		}
	}

	sort.Slice(expanded, func(i, j int) bool { return expanded[i].score > expanded[j].score })
	if len(expanded) > limit {
		expanded = expanded[:limit]
	}
	return expanded
}

// recallTimeWindow returns records within (lower, upper] honouring the
// Kinds/Tags filters from q. Used by expandTemporally; not a public
// surface.
func (s *store) recallTimeWindow(ctx context.Context, lower, upper time.Time, q hippo.MemoryQuery) ([]*scoredRecord, error) {
	now := time.Now().UnixNano()
	args := []any{now, now, lower.UnixNano(), upper.UnixNano()}

	var sb strings.Builder
	fmt.Fprintf(&sb, `
		SELECT m.id, m.kind, m.timestamp, m.content, m.importance,
		       COALESCE(m.embedding, X''), 0.0,
		       %s
		FROM memories m
		WHERE m.timestamp BETWEEN ? AND ?`, effectiveImportanceExpr)

	// Re-apply Kinds and Tags but NOT Since/Until (we just scoped by
	// our own window). Also skip MinImportance — neighbors take the
	// hit's score directly.
	qWindow := hippo.MemoryQuery{Kinds: q.Kinds, Tags: q.Tags}
	applyCommonFilters(&sb, &args, qWindow)
	sb.WriteString(" ORDER BY m.timestamp DESC LIMIT 50")

	rows, err := s.scanScored(ctx, sb.String(), args, scanModeRecency)
	if err != nil {
		return nil, err
	}
	out := make([]*scoredRecord, len(rows))
	for i := range rows {
		rr := rows[i]
		out[i] = &rr
	}
	return out, nil
}

// applyCommonFilters appends Kinds/Tags/Since/Until/MinImportance
// predicates to sb and corresponding args. MinImportance runs against
// the effective-importance expression so decay wins out over base.
func applyCommonFilters(sb *strings.Builder, args *[]any, q hippo.MemoryQuery) {
	if len(q.Kinds) > 0 {
		sb.WriteString(" AND m.kind IN (")
		for i, k := range q.Kinds {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
			*args = append(*args, string(k))
		}
		sb.WriteByte(')')
	}

	if len(q.Tags) > 0 {
		sb.WriteString(" AND m.id IN (SELECT memory_id FROM tags WHERE tag IN (")
		for i, t := range q.Tags {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
			*args = append(*args, t)
		}
		sb.WriteString("))")
	}

	if !q.Since.IsZero() {
		sb.WriteString(" AND m.timestamp >= ?")
		*args = append(*args, q.Since.UnixNano())
	}
	if !q.Until.IsZero() {
		sb.WriteString(" AND m.timestamp <= ?")
		*args = append(*args, q.Until.UnixNano())
	}
	// MinImportance runs against the decayed effective importance,
	// which scanScored writes into rec.Importance. We filter the
	// post-scan slice (applyMinImportance) rather than complicate the
	// SQL with a HAVING/WITH wrapper.
}

type scanMode int

const (
	scanModeKeyword scanMode = iota
	scanModeRecency
	scanModeSemantic
)

// scanScored executes sql and decodes rows into scoredRecord values.
// The SELECT column order must be:
//   id, kind, timestamp(ns), content, importance, embedding_bytes,
//   keyword_score, effective_importance
//
// scanScored applies no filter on q.MinImportance itself — the caller
// passes that through q on the wrapper function and scanScored
// discards rows whose effective importance falls below the cutoff.
func (s *store) scanScored(ctx context.Context, query string, args []any, mode scanMode) ([]scoredRecord, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: recall: %w", err)
	}
	defer rows.Close()

	var out []scoredRecord
	for rows.Next() {
		var (
			r         scoredRecord
			kind      string
			tsNano    int64
			embBytes  []byte
			keyScore  sql.NullFloat64
			effective sql.NullFloat64
		)
		if err := rows.Scan(&r.rec.ID, &kind, &tsNano, &r.rec.Content, &r.rec.Importance,
			&embBytes, &keyScore, &effective); err != nil {
			return nil, fmt.Errorf("memory/sqlite: recall scan: %w", err)
		}
		r.rec.Kind = hippo.MemoryKind(kind)
		r.rec.Timestamp = time.Unix(0, tsNano)
		if len(embBytes) > 0 {
			if v, err := decodeEmbedding(embBytes); err == nil {
				r.embedding = v
				r.rec.Embedding = v
			}
		}
		// bm25() returns negative numbers (lower = better in SQLite's
		// ranking convention). We flip the sign and take a bounded
		// value so score stays comparable with semantic cosine.
		if mode == scanModeKeyword && keyScore.Valid {
			r.keyword = bm25ToScore(keyScore.Float64)
		}
		if effective.Valid {
			r.rec.Importance = clamp01(effective.Float64)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory/sqlite: recall iter: %w", err)
	}
	return out, nil
}

// bm25ToScore turns SQLite FTS5's bm25 output (negative floats, lower
// = more relevant) into a [0, 1] score. 1/(1+|bm25|) is a crude but
// monotonic squash that keeps ranking stable.
func bm25ToScore(b float64) float64 {
	if b == 0 {
		return 0
	}
	absB := math.Abs(b)
	return 1.0 / (1.0 + absB)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// markAccessed updates last_accessed and bumps access_count for the
// supplied IDs. Runs best-effort — a failure is logged at Warn but
// doesn't fail Recall.
func (s *store) markAccessed(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	now := time.Now().UnixNano()
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for _, id := range ids {
		args = append(args, id)
	}
	query := fmt.Sprintf(`UPDATE memories SET last_accessed = ?,
	                        access_count = COALESCE(access_count, 0) + 1
	                      WHERE id IN (%s)`, placeholders)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		s.logger.Warn("memory/sqlite: markAccessed failed", "err", err)
	}
}
