package sqlite

import (
	"encoding/binary"
	"errors"
	"math"
)

// encodeEmbedding packs a float32 slice into little-endian bytes for
// storage in a BLOB column. 4 bytes per element, no header — the
// column has no use for a length prefix because SQLite already tells
// us the byte length on scan.
func encodeEmbedding(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeEmbedding unpacks bytes produced by encodeEmbedding. Returns
// an error if the byte length isn't a multiple of 4 — that would mean
// the BLOB was written by something other than encodeEmbedding or
// the database is corrupted.
func decodeEmbedding(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b)%4 != 0 {
		return nil, errors.New("memory/sqlite: embedding blob length is not a multiple of 4")
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// cosineSimilarity returns the cosine of the angle between a and b in
// [-1, 1]. Returns 0 when either input has zero norm (rather than
// NaN) so callers can treat the "no signal" case uniformly. Returns 0
// on mismatched lengths so one rogue record with a different model's
// vectors can't poison a whole recall.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
