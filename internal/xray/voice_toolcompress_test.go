package xray

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/headroom"
)

// TestCompressToolResult_StripsMarkdownWhenUnderBudget is the regression test
// for the ADR-038 strip leak: LossyCompress strips markdown noise even when the
// body fits under the budget (truncated=false), but an earlier caller only
// adopted the crushed form when it also truncated — so the strip saving (the
// dominant win on this corpus) was discarded on every under-budget result. This
// path serves the built-in, native recall, and remote-MCP tools alike.
func TestCompressToolResult_StripsMarkdownWhenUnderBudget(t *testing.T) {
	body := "See the [docs](https://example.com/a/very/long/url/that/costs/tokens) here."
	got := compressToolResult(body, "get_graph_node", 500, zerolog.Nop())
	if strings.Contains(got, "https://") {
		t.Errorf("link URL should be stripped even when under budget: %q", got)
	}
	if !strings.Contains(got, "docs") {
		t.Errorf("link text should survive: %q", got)
	}
	if len(got) >= len(body) {
		t.Errorf("under-budget markdown should still shrink: in=%d out=%d", len(body), len(got))
	}
}

// TestCompressToolResult_TruncatesOverBudget covers the cap engaging on a large
// result (the get_code_node-returns-whole-ADR case).
func TestCompressToolResult_TruncatesOverBudget(t *testing.T) {
	body := strings.Repeat("word ", 500) // ~2500 chars
	got := compressToolResult(body, "get_graph_node", 100, zerolog.Nop())
	if n := len([]rune(got)); n > 110 {
		t.Errorf("over-budget result not capped near limit: %d runes", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated result should carry the ellipsis marker: %q", got)
	}
}

// TestCompressToolResult_ZeroLimitPassesThrough confirms the feature stays
// opt-in: limit<=0 returns the result verbatim (no strip, no truncation).
func TestCompressToolResult_ZeroLimitPassesThrough(t *testing.T) {
	body := "# raw [markdown](url) kept verbatim"
	if got := compressToolResult(body, "x", 0, zerolog.Nop()); got != body {
		t.Errorf("limit<=0 must pass through unchanged, got %q", got)
	}
}

// compressItemRows is the shared list-tool result builder (search_memory,
// get_pinned_items, get_agent_notes). The key contract it must uphold — and that
// the native code-tool char cap got WRONG — is that it compresses each row's
// content but NEVER drops a row: count stays N. These cover that plus factoring
// and the extra-field merge.

type itemRowsResult struct {
	Count  int               `json:"count"`
	Query  string            `json:"query,omitempty"`
	Common map[string]string `json:"common,omitempty"`
	Items  []struct {
		ID        string `json:"id"`
		Kind      string `json:"kind,omitempty"`
		Title     string `json:"title"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated,omitempty"`
	} `json:"items"`
}

// TestCompressItemRows_CompressesContentButKeepsAllRows is the central
// guarantee: every row survives (no list truncation), long bodies are crushed,
// and the truncated flag is set per row.
func TestCompressItemRows_CompressesContentButKeepsAllRows(t *testing.T) {
	long := strings.Repeat("word ", 300) // ~1500 chars, over a 100 budget
	in := []headroom.Row{
		{ID: "1", Kind: "note", Title: "A", Content: long},
		{ID: "2", Kind: "note", Title: "B", Content: long},
		{ID: "3", Kind: "note", Title: "C", Content: "short body"},
	}
	out := compressItemRows("list_memory", nil, in, 100, zerolog.Nop())

	var got itemRowsResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out)
	}
	if got.Count != 3 || len(got.Items) != 3 {
		t.Fatalf("ALL rows must survive (no list truncation): count=%d items=%d", got.Count, len(got.Items))
	}
	for _, it := range got.Items[:2] {
		if !it.Truncated {
			t.Errorf("row %s over budget should be truncated", it.ID)
		}
		if n := len([]rune(it.Content)); n > 110 {
			t.Errorf("row %s content not capped near budget: %d runes", it.ID, n)
		}
	}
}

// TestCompressItemRows_FactorsUniformKind hoists a field uniform across all rows
// into common{} and merges the extra map (search's query) into the envelope.
func TestCompressItemRows_FactorsUniformKind(t *testing.T) {
	in := []headroom.Row{
		{ID: "1", Kind: "note", Title: "A", Content: "x"},
		{ID: "2", Kind: "note", Title: "B", Content: "y"},
	}
	out := compressItemRows("search_memory", map[string]any{"query": "q"}, in, 500, zerolog.Nop())

	var got itemRowsResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if got.Query != "q" {
		t.Errorf("extra field (query) should merge into the envelope, got %q", got.Query)
	}
	if got.Common["kind"] != "note" {
		t.Errorf("uniform kind should be factored into common, got %v", got.Common)
	}
	for _, it := range got.Items {
		if it.Kind != "" {
			t.Errorf("factored kind should be blank on row %s, got %q", it.ID, it.Kind)
		}
	}
}

// TestCompressItemRows_ZeroLimitPassesThrough confirms passthrough keeps full
// content (used by tests and when the budget is disabled).
func TestCompressItemRows_ZeroLimitPassesThrough(t *testing.T) {
	long := strings.Repeat("z", 2000)
	in := []headroom.Row{{ID: "1", Kind: "note", Title: "A", Content: long}}
	out := compressItemRows("fetch_memory", nil, in, 0, zerolog.Nop())

	var got itemRowsResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if got.Count != 1 || len(got.Items[0].Content) != 2000 {
		t.Errorf("limit<=0 must keep full content: count=%d len=%d", got.Count, len(got.Items[0].Content))
	}
}
