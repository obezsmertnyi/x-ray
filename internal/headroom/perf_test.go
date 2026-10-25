package headroom

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// sampleCorpus mimics a realistic search_knowledge / search_memory result set:
// markdown-heavy bodies (headings, links, images, code fences, lists) of the
// kind blog posts and project descriptions carry, plus a couple of near-
// duplicates that adaptive-K should collapse.
func sampleCorpus() []Row {
	body := func(topic string) string {
		return "# " + topic + " Overview\n\n" +
			"This is the canonical writeup for **" + topic + "**. " +
			"See the [design doc](https://docs.example.com/" + topic + "/design/v3/full) and " +
			"the [runbook](https://wiki.internal.example.com/runbooks/" + topic + ").\n\n" +
			"![architecture diagram](https://cdn.example.com/diagrams/" + topic + "-arch-v12.png)\n\n" +
			"## Key points\n\n" +
			"- It integrates with the upstream pipeline over gRPC.\n" +
			"- Retries use exponential backoff capped at thirty seconds.\n" +
			"- Metrics are exported to the central Prometheus instance.\n\n" +
			"```go\nfunc handle(ctx context.Context, r *Request) (*Response, error) {\n" +
			"\treturn process(ctx, r) // hot path, keep allocations low\n}\n```\n\n" +
			"> Note: the legacy v1 endpoint is deprecated and removed in the next release.\n\n" +
			strings.Repeat("Additional background prose that the model rarely needs verbatim. ", 8)
	}
	// Six distinct topics plus six near-duplicates of "ingest" — twelve rows
	// puts the set past the n<=8 fast path so adaptive-K (SimHash dedup) is
	// genuinely exercised, mirroring an over-broad query that returns many
	// near-identical hits.
	topics := []string{"ingest", "vectorstore", "voice", "auth", "graph", "embeddings"}
	rows := make([]Row, 0, 12)
	for i, tp := range topics {
		rows = append(rows, Row{ID: fmt.Sprintf("%d", i+1), Kind: "stable_fact", Scope: "public", Title: strings.ToUpper(tp[:1]) + tp[1:], Content: body(tp)})
	}
	for i := 0; i < 6; i++ {
		rows = append(rows, Row{ID: fmt.Sprintf("d%d", i), Kind: "stable_fact", Scope: "public", Title: fmt.Sprintf("Ingest dup %d", i), Content: body("ingest")})
	}
	return rows
}

// serializedLen mirrors how the tool handlers emit results: marshal the rows
// (plus the factored header) to JSON and measure bytes — a close proxy for the
// token count the LLM is billed for (~4 chars/token).
func serializedLen(rows []Row, constant map[string]string) int {
	type wire struct {
		ID        string `json:"id"`
		Kind      string `json:"kind,omitempty"`
		Scope     string `json:"scope,omitempty"`
		Title     string `json:"title"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated,omitempty"`
	}
	w := make([]wire, len(rows))
	for i, r := range rows {
		w[i] = wire{r.ID, r.Kind, r.Scope, r.Title, r.Content, r.Truncated}
	}
	b, _ := json.Marshal(map[string]any{"items": w, "common": constant})
	return len(b)
}

// TestCompression_RealReduction proves the compression actually shrinks the
// serialized payload on realistic content, and fails if it ever regresses below
// a meaningful threshold — so "does it really work?" stays answered in CI.
func TestCompression_RealReduction(t *testing.T) {
	corpus := sampleCorpus()
	before := serializedLen(corpus, nil)

	res := CompressRows(corpus, Config{MaxContentChars: 500})
	after := serializedLen(res.Rows, res.Constant)

	saved := before - after
	pct := float64(saved) / float64(before) * 100

	t.Logf("payload: %d → %d bytes (%.1f%% smaller, ~%d tokens saved); rows %d",
		before, after, pct, saved/4, len(res.Rows))

	// LossyCompress + factor on markdown-heavy bodies. The win is from
	// markdown-stripping + 500-char truncation; rows are not dropped.
	if pct < 40 {
		t.Errorf("expected >40%% reduction on markdown-heavy corpus, got %.1f%%", pct)
	}
	if len(res.Rows) != len(corpus) {
		t.Errorf("LossyCompress must not drop rows: %d → %d", len(corpus), len(res.Rows))
	}
	if res.Constant["kind"] != "stable_fact" {
		t.Errorf("expected uniform kind to be factored out, got %v", res.Constant)
	}
}

// TestLossyCompress_ReductionOnLongBody isolates the per-field win.
func TestLossyCompress_ReductionOnLongBody(t *testing.T) {
	body := sampleCorpus()[0].Content
	preview, truncated := LossyCompress(body, 500)
	if !truncated {
		t.Fatal("expected the long sample body to be truncated")
	}
	pct := float64(len(body)-len(preview)) / float64(len(body)) * 100
	t.Logf("single body: %d → %d chars (%.1f%% smaller)", len(body), len(preview), pct)
	if len(preview) >= len(body) {
		t.Error("preview must be shorter than the original")
	}
}

// TestComparison_BeforeAfter prints a side-by-side table of the serialized
// payload size with compression OFF (the pre-change baseline) vs ON, at a few
// budgets, so the win is reportable in concrete numbers. It is a measurement
// harness as much as a guard: it fails only if ON is ever larger than OFF.
func TestComparison_BeforeAfter(t *testing.T) {
	corpus := sampleCorpus()

	// OFF: zero Config — identical to the behaviour before ADR-038.
	off := CompressRows(corpus, Config{})
	offBytes := serializedLen(off.Rows, off.Constant)

	t.Logf("corpus: %d rows of markdown-heavy memory content", len(corpus))
	t.Logf("%-32s %8s %8s %8s %9s %8s", "config", "rows", "bytes", "~tokens", "saved", "vs OFF")
	t.Logf("%-32s %8d %8d %8d %9s %8s", "OFF (baseline, no changes)", len(off.Rows), offBytes, offBytes/4, "—", "—")

	cases := []struct {
		name string
		cfg  Config
	}{
		{"LossyCompress+factor (limit=500)", Config{MaxContentChars: 500}},
		{"LossyCompress+factor (limit=200)", Config{MaxContentChars: 200}},
	}
	for _, c := range cases {
		res := CompressRows(append([]Row(nil), corpus...), c.cfg)
		onBytes := serializedLen(res.Rows, res.Constant)
		pct := float64(offBytes-onBytes) / float64(offBytes) * 100
		t.Logf("%-32s %8d %8d %8d %9d %7.1f%%", c.name, len(res.Rows), onBytes, onBytes/4, offBytes-onBytes, pct)
		if onBytes > offBytes {
			t.Errorf("%s: ON payload (%d) larger than OFF (%d)", c.name, onBytes, offBytes)
		}
	}
}

func BenchmarkCompressRows(b *testing.B) {
	corpus := sampleCorpus()
	cfg := Config{MaxContentChars: 500}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = CompressRows(append([]Row(nil), corpus...), cfg)
	}
}
