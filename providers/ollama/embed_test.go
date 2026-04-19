package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbedderNameAndDefaults(t *testing.T) {
	e := NewEmbedder()
	if got, want := e.Name(), "ollama:nomic-embed-text"; got != want {
		t.Errorf("Name = %q; want %q", got, want)
	}
	if e.Dimensions() != 0 {
		t.Errorf("pre-call Dimensions = %d; want 0", e.Dimensions())
	}
}

func TestEmbedBatchHappyPath(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := make([][]float32, len(body.Input))
		for i := range out {
			out[i] = []float32{float32(i) + 0.1, float32(i) + 0.2, float32(i) + 0.3}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	}))
	defer srv.Close()

	e := NewEmbedder(WithEmbedderBaseURL(srv.URL))
	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 3 || len(vecs[0]) != 3 {
		t.Fatalf("unexpected shape: %v", vecs)
	}
	if e.Dimensions() != 3 {
		t.Errorf("Dimensions = %d; want 3", e.Dimensions())
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 batch call, got %d", calls.Load())
	}
}

func TestEmbedSingleFallback(t *testing.T) {
	var batchCalls, singleCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			batchCalls.Add(1)
			http.NotFound(w, r)
		case "/api/embeddings":
			singleCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 2}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	e := NewEmbedder(WithEmbedderBaseURL(srv.URL))
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("vecs = %v", vecs)
	}
	if batchCalls.Load() != 1 {
		t.Errorf("batchCalls = %d; want 1 (first try)", batchCalls.Load())
	}
	if singleCalls.Load() != 2 {
		t.Errorf("singleCalls = %d; want 2 (fallback loop)", singleCalls.Load())
	}
	// Subsequent calls skip the batch probe.
	batchCalls.Store(0)
	singleCalls.Store(0)
	if _, err := e.Embed(context.Background(), []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if batchCalls.Load() != 0 {
		t.Errorf("second-call batchCalls = %d; want 0 (preferSingle set)", batchCalls.Load())
	}
}

func TestEmbedMismatchedCountReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{{1, 2}},
		})
	}))
	defer srv.Close()
	e := NewEmbedder(WithEmbedderBaseURL(srv.URL))
	if _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("want mismatched-count error")
	} else if !strings.Contains(err.Error(), "vectors for") {
		t.Errorf("err = %v", err)
	}
}

func TestEmbedHonoursContextCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	e := NewEmbedder(WithEmbedderBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.Embed(ctx, []string{"a"}); err == nil {
		t.Fatal("want ctx-related error")
	}
}

func TestEmbedEmptyInputNoCall(t *testing.T) {
	// An Embed with no texts must not touch the network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer srv.Close()
	e := NewEmbedder(WithEmbedderBaseURL(srv.URL))
	vecs, err := e.Embed(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Fatalf("empty Embed = (%v, %v)", vecs, err)
	}
}
