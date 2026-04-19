package sqlite

import (
	"math"
	"testing"
)

func TestEmbeddingRoundTrip(t *testing.T) {
	cases := [][]float32{
		{},
		{0},
		{1.5, -2.25, 0.0, 3.14159},
		{float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN())},
	}
	for _, c := range cases {
		b := encodeEmbedding(c)
		got, err := decodeEmbedding(b)
		if err != nil {
			t.Fatalf("decode %v: %v", c, err)
		}
		if len(got) != len(c) {
			t.Fatalf("decode length %d != encode length %d", len(got), len(c))
		}
		for i := range c {
			if math.IsNaN(float64(c[i])) {
				if !math.IsNaN(float64(got[i])) {
					t.Errorf("lost NaN at %d", i)
				}
				continue
			}
			if got[i] != c[i] {
				t.Errorf("index %d: got %v want %v", i, got[i], c[i])
			}
		}
	}
}

func TestDecodeRejectsOddLengthBytes(t *testing.T) {
	if _, err := decodeEmbedding([]byte{1, 2, 3}); err == nil {
		t.Fatal("want error")
	}
}

func TestCosineSimilarityEdgeCases(t *testing.T) {
	if got := cosineSimilarity(nil, nil); got != 0 {
		t.Errorf("nil,nil: got %v want 0", got)
	}
	if got := cosineSimilarity([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("mismatched lengths: got %v want 0", got)
	}
	if got := cosineSimilarity([]float32{0, 0, 0}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("zero vector: got %v want 0", got)
	}
	same := cosineSimilarity([]float32{1, 2, 3}, []float32{1, 2, 3})
	if math.Abs(float64(same-1)) > 1e-6 {
		t.Errorf("identical vectors: got %v want ~1", same)
	}
	ortho := cosineSimilarity([]float32{1, 0}, []float32{0, 1})
	if math.Abs(float64(ortho)) > 1e-6 {
		t.Errorf("orthogonal vectors: got %v want ~0", ortho)
	}
	opp := cosineSimilarity([]float32{1, 2, 3}, []float32{-1, -2, -3})
	if math.Abs(float64(opp+1)) > 1e-6 {
		t.Errorf("opposite vectors: got %v want ~-1", opp)
	}
}
