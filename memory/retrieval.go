// Package memory holds utilities shared by memory backends that
// implement the hippo.Memory interface. The interface itself, along with
// the Record, MemoryKind, and Scope types, lives in the root hippo
// package.
//
// Backends live in subdirectories (memory/sqlite, etc.) and import
// github.com/mahdi-salmanzade/hippo for the interface and types.
package memory

import (
	"context"

	"github.com/mahdi-salmanzade/hippo"
)

// NucleusRetrieval ranks candidate records by relevance to query and
// returns the smallest prefix whose cumulative importance-weighted score
// meets threshold p (analogous to nucleus / top-p sampling in language
// models).
//
// The goal is to return "enough" context without flooding the prompt: a
// broad, shallow Recall feeds this function, which trims to the
// informationally dense head of the distribution.
//
// Backends may use this as their default ranking step after an initial
// vector or lexical search.
func NucleusRetrieval(ctx context.Context, query string, candidates []hippo.Record, p float64) ([]hippo.Record, error) {
	_ = ctx
	_ = query
	_ = candidates
	_ = p
	// TODO: score each candidate (embedding cosine if available, BM25
	// otherwise), sort descending, accumulate weights until p is
	// reached, return prefix.
	return nil, nil
}
