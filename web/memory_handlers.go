package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
)

// memoryPageData is what memory.html renders. Pagination is naïve
// (LIMIT/OFFSET) because the total set is expected to stay small for
// a single user; adding a cursor is a v0.3 improvement.
type memoryPageData struct {
	Records  []memoryRow
	Query    string
	Mode     string // keyword | semantic | hybrid | recent
	Page     int
	HasNext  bool
	Backfill memoryBackfillView
	TotalRows int64
}

type memoryRow struct {
	ID         string
	Kind       string
	Timestamp  time.Time
	Content    string
	Tags       []string
	Importance float64
}

type memoryBackfillView struct {
	Total      int64
	Embedded   int64
	Pending    int64
	Running    bool
	LastRunAt  string
	LastError  string
	HasEmbedder bool
}

const memoryPageSize = 20

// handleMemoryGet renders /memory — the browse-and-search page.
func (s *Server) handleMemoryGet(w http.ResponseWriter, r *http.Request) {
	if s.Bundle() == nil || s.Bundle().Memory == nil {
		flash, flashErr := readFlash(w, r)
		s.render(w, "memory.html", pageData{
			Title:    "Memory",
			Active:   "memory",
			Flash:    flash,
			FlashErr: flashErr,
			Data: memoryPageData{
				Backfill: memoryBackfillView{},
			},
		})
		return
	}

	page := 0
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	query := r.URL.Query().Get("q")
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "recent"
	}

	rows, hasNext, err := s.loadMemoryRows(r.Context(), query, mode, page)
	if err != nil {
		http.Error(w, "memory error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	bfView := s.loadBackfillView(r.Context())
	flash, flashErr := readFlash(w, r)
	s.render(w, "memory.html", pageData{
		Title:    "Memory",
		Active:   "memory",
		Flash:    flash,
		FlashErr: flashErr,
		Data: memoryPageData{
			Records:   rows,
			Query:     query,
			Mode:      mode,
			Page:      page,
			HasNext:   hasNext,
			Backfill:  bfView,
			TotalRows: bfView.Total,
		},
	})
}

// loadMemoryRows pulls up to memoryPageSize+1 records (the +1 lets us
// decide whether there's a next page without a COUNT query).
func (s *Server) loadMemoryRows(ctx context.Context, query, mode string, page int) ([]memoryRow, bool, error) {
	b := s.Bundle()
	if b == nil || b.Memory == nil {
		return nil, false, nil
	}

	q := hippo.MemoryQuery{Limit: memoryPageSize + 1}
	switch mode {
	case "keyword":
		// Plain FTS5 — set semantic=false, use query as-is.
	case "semantic":
		q.Semantic = true
	case "hybrid":
		q.Semantic = true
		q.HybridWeight = 0.6
		q.TemporalExpansion = 30 * time.Minute
	case "recent":
		// No query — fall through to recency path.
		query = ""
	}

	// LIMIT+OFFSET via a raw query when paginating — hippo.MemoryQuery
	// doesn't expose offset. For v0.2 paginate by re-running with
	// higher limits and slicing; this mirrors how semantic expansion
	// returns denormalised sets anyway.
	q.Limit = (page + 1) * (memoryPageSize + 1)
	recs, err := b.Memory.Recall(ctx, query, q)
	if err != nil {
		return nil, false, err
	}

	// Paginate client-side.
	start := page * memoryPageSize
	if start >= len(recs) {
		return nil, false, nil
	}
	end := start + memoryPageSize
	hasNext := end < len(recs)
	if end > len(recs) {
		end = len(recs)
	}
	slice := recs[start:end]

	out := make([]memoryRow, 0, len(slice))
	for _, r := range slice {
		content := r.Content
		if len(content) > 400 {
			content = content[:400] + "…"
		}
		out = append(out, memoryRow{
			ID:         r.ID,
			Kind:       string(r.Kind),
			Timestamp:  r.Timestamp,
			Content:    content,
			Tags:       r.Tags,
			Importance: r.Importance,
		})
	}
	return out, hasNext, nil
}

// loadBackfillView reads status from the sqlite store if it supports
// the backfillStatusReader interface. A store without the interface
// (future backends) just returns a view with HasEmbedder=false.
type backfillStatusReader interface {
	BackfillStatus(ctx context.Context) (sqlite.BackfillStatus, error)
	Embedder() hippo.Embedder
}

func (s *Server) loadBackfillView(ctx context.Context) memoryBackfillView {
	b := s.Bundle()
	if b == nil || b.Memory == nil {
		return memoryBackfillView{}
	}
	reader, ok := b.Memory.(backfillStatusReader)
	if !ok {
		return memoryBackfillView{}
	}
	st, err := reader.BackfillStatus(ctx)
	if err != nil {
		return memoryBackfillView{LastError: err.Error()}
	}
	view := memoryBackfillView{
		Total:       st.Total,
		Embedded:    st.Embedded,
		Pending:     st.Pending,
		Running:     st.Running,
		LastError:   st.LastError,
		HasEmbedder: reader.Embedder() != nil,
	}
	if !st.LastRunAt.IsZero() {
		view.LastRunAt = st.LastRunAt.Local().Format("15:04:05")
	}
	return view
}

// handleMemoryBackfillFragment renders the right-sidebar progress
// widget. Targeted by htmx polling every 5 seconds.
func (s *Server) handleMemoryBackfillFragment(w http.ResponseWriter, r *http.Request) {
	view := s.loadBackfillView(r.Context())
	s.renderFragment(w, "fragments/memory_backfill.html", view)
}

// handleMemoryPrune is the manual-prune button. Runs pruneOnce with
// the current config's policy and redirects back.
func (s *Server) handleMemoryPrune(w http.ResponseWriter, r *http.Request) {
	b := s.Bundle()
	if b == nil || b.Memory == nil {
		http.Error(w, "no memory configured", http.StatusServiceUnavailable)
		return
	}
	pruner, ok := b.Memory.(interface {
		Prune(ctx context.Context, before time.Time) error
	})
	if !ok {
		http.Error(w, "memory backend does not support prune", http.StatusNotImplemented)
		return
	}
	// Manual prune calls the legacy hippo.Memory.Prune which targets
	// Working rows older than 1 day — a predictable, conservative
	// action for a button click.
	if err := pruner.Prune(r.Context(), time.Now().Add(-24*time.Hour)); err != nil {
		writeFlash(w, "", "prune failed: "+err.Error())
	} else {
		writeFlash(w, "Pruned Working records older than 1 day.", "")
	}
	http.Redirect(w, r, "/memory", http.StatusSeeOther)
}

// idDeleter is the narrow interface memory backends implement to
// support single-record removal from the web UI. The sqlite store
// satisfies it via DeleteByID; other backends can add the same shape.
type idDeleter interface {
	DeleteByID(ctx context.Context, id string) error
}

// handleMemoryDelete removes one record by id and bounces back to
// /memory. Guarded by a confirm button in the template.
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	b := s.Bundle()
	if b == nil || b.Memory == nil {
		http.Error(w, "no memory configured", http.StatusServiceUnavailable)
		return
	}
	deleter, ok := b.Memory.(idDeleter)
	if !ok {
		http.Error(w, "delete not supported by memory backend", http.StatusNotImplemented)
		return
	}
	if err := deleter.DeleteByID(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeFlash(w, "Record removed.", "")
	http.Redirect(w, r, "/memory", http.StatusSeeOther)
}
