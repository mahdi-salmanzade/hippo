package memory

import "time"

// Record is one entry in the memory store.
//
// Content is kept in its raw form; hippo deliberately does not summarise
// content before storage, to preserve fidelity for later retrieval. If
// summarisation is desired it should happen at Recall time or in the
// application layer.
type Record struct {
	// ID uniquely identifies the record. Empty on Add; the backend
	// assigns one.
	ID string
	// Kind is the temporal role of this record.
	Kind Kind
	// Timestamp is when the event occurred (not when it was stored).
	Timestamp time.Time
	// Content is the raw text. Not pre-processed.
	Content string
	// Tags are arbitrary labels for filtering and retrieval.
	Tags []string
	// Importance is a 0..1 heuristic used to weight retrieval and to
	// exempt records from Prune when high enough. Backend-specific.
	Importance float64
	// Embedding is an optional vector representation of Content. Filled
	// lazily by backends that support embeddings. A non-nil Embedding
	// must match the backend's configured dimensionality.
	Embedding []float32
	// Source optionally identifies the origin of the record (e.g. a
	// Call ID, a conversation ID, a file path).
	Source string
}
