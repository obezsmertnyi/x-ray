package engine

// Test doubles shared by the cgo and !cgo test files. They live in an untagged
// file on purpose: registry_test.go is cgo-only (it parses fixtures with
// Tree-sitter), and a helper defined there is invisible to a static-build test,
// which is exactly the build the memory-note path must be proven in.

import (
	"context"
	"hash/fnv"

	"github.com/rs/zerolog"
)

// stubEmbedder returns deterministic, text-derived vectors so tests need no
// network. It records whether EmbedBatch (node embedding) was ever called — the
// snapshot load path must never call it.
type stubEmbedder struct {
	dims        int
	batchCalled bool
	panicBatch  bool // when true, EmbedBatch panics — proves the load path never embeds nodes
	panicQuery  bool // when true, EmbedQuery panics — proves a -no-embed build never embeds its summary either
}

func (s *stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	if s.panicBatch {
		panic("EmbedBatch must not be called on the snapshot load path")
	}
	s.batchCalled = true
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = vecFor(t, s.dims)
	}
	return out, nil
}

func (s *stubEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	if s.panicQuery {
		panic("EmbedQuery must not be called building or loading a -no-embed snapshot")
	}
	return vecFor(text, s.dims), nil
}

// vecFor derives a stable unit-ish vector from text so similar text → similar
// vector enough for routing assertions (each test project uses a distinct token).
func vecFor(text string, dims int) []float32 {
	v := make([]float32, dims)
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	seed := h.Sum32()
	for i := range v {
		seed = seed*1664525 + 1013904223
		v[i] = float32(int32(seed)) / float32(1<<31)
	}
	// Bias dimension 0 by a token so "alpha"-tagged text clusters away from "beta".
	if containsToken(text, "alpha") {
		v[0] += 5
	}
	if containsToken(text, "beta") {
		v[1] += 5
	}
	return v
}

func containsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if s[i:i+len(tok)] == tok {
			return true
		}
	}
	return false
}

func testLogger() zerolog.Logger { return zerolog.Nop() }
