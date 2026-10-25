package main

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
)

// hashEmbedder derives a deterministic unit vector from text — enough for a
// build to run without an embeddings server.
type hashEmbedder struct{ dims int }

func (e hashEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, e.dims)
	for i := range v {
		h := fnv.New32a()
		h.Write([]byte(text))
		h.Write([]byte{byte(i)})
		v[i] = float32(h.Sum32()%1000)/500 - 1
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v, nil
}

func (e hashEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, _ := e.EmbedQuery(ctx, t)
		out[i] = v
	}
	return out, nil
}

// -summary is what makes a catalog map routable: without it the header carries
// the stats line, with it the operator's description, marked "explicit".
func TestServiceMap_SummarySetsRoutingDescription(t *testing.T) {
	entries := []catalogEntry{
		{Name: "alpha", Kind: "Student", Text: "alpha. field engineer. 7 years."},
		{Name: "beta", Kind: "Student", Text: "beta. platform engineer. 12 years."},
	}
	fp := engine.EmbedFingerprint{Model: "stub", Dims: 8, MaxInputChars: 512}
	emb := hashEmbedder{dims: 8}
	ctx := context.Background()

	plain, st, err := buildCatalogSnapshot(ctx, entries, "cat", "cat.json", "", emb, fp, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if st.nodes != 2 {
		t.Fatalf("two entries → two nodes, got %d", st.nodes)
	}
	if plain.Header.SummarySource != "stats" {
		t.Errorf("no -summary must fall back to stats, got %q (%q)", plain.Header.SummarySource, plain.Header.Summary)
	}

	want := "students of the team course: who they are, their experience and tools"
	described, _, err := buildCatalogSnapshot(ctx, entries, "cat", "cat.json", "  "+want+"  ", emb, fp, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if described.Header.SummarySource != "explicit" {
		t.Errorf("-summary must be recorded as explicit, got %q", described.Header.SummarySource)
	}
	if !strings.Contains(described.Header.Summary, want) || !strings.HasPrefix(described.Header.Summary, "cat: ") {
		t.Errorf("summary must carry the label and the operator text, got %q", described.Header.Summary)
	}
	if len(described.Header.SummaryVec) != fp.Dims {
		t.Errorf("routing vector must be embedded at the fingerprint dims, got %d", len(described.Header.SummaryVec))
	}
}
