package headroom

import (
	"strings"
	"testing"
)

func TestLossyCompress_StripsMarkdownNoise(t *testing.T) {
	body := "# Heading\n\nSee the [docs](https://example.com/very/long/url) and " +
		"![logo](https://cdn.example.com/logo.png) for details.\n\n- item one\n- item two"
	got, truncated := LossyCompress(body, 500)
	if truncated {
		t.Fatalf("did not expect truncation for short body, got truncated=true: %q", got)
	}
	if strings.Contains(got, "https://") {
		t.Errorf("link/image URLs should be stripped: %q", got)
	}
	if strings.Contains(got, "#") || strings.Contains(got, "- ") {
		t.Errorf("line markers should be stripped: %q", got)
	}
	for _, want := range []string{"Heading", "docs", "logo", "item one", "item two"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q to survive stripping: %q", want, got)
		}
	}
}

func TestLossyCompress_StripsFencedCode(t *testing.T) {
	body := "Intro text.\n\n```go\nfunc main() { println(\"hello\") }\n```\n\nOutro."
	got, _ := LossyCompress(body, 500)
	if strings.Contains(got, "func main") {
		t.Errorf("fenced code body should be replaced: %q", got)
	}
	if !strings.Contains(got, "«code»") {
		t.Errorf("expected code placeholder: %q", got)
	}
	if !strings.Contains(got, "Intro") || !strings.Contains(got, "Outro") {
		t.Errorf("surrounding prose should survive: %q", got)
	}
}

func TestLossyCompress_TruncatesAtWordBoundary(t *testing.T) {
	body := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	got, truncated := LossyCompress(body, 20)
	if !truncated {
		t.Fatalf("expected truncation, got %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix: %q", got)
	}
	trimmed := strings.TrimSuffix(got, "…")
	if strings.HasSuffix(trimmed, "gamm") || strings.Contains(trimmed, "gam ") {
		t.Errorf("word should not be split mid-token: %q", got)
	}
	if []rune(trimmed)[len([]rune(trimmed))-1] == ' ' {
		t.Errorf("trailing space before ellipsis: %q", got)
	}
}

func TestLossyCompress_ZeroLimitPassesThrough(t *testing.T) {
	body := "# raw [markdown](url) kept verbatim"
	got, truncated := LossyCompress(body, 0)
	if got != body || truncated {
		t.Errorf("maxChars<=0 must pass through unchanged, got %q truncated=%v", got, truncated)
	}
}

func TestLossyCompress_HardCutsUnbrokenToken(t *testing.T) {
	body := strings.Repeat("x", 100)
	got, truncated := LossyCompress(body, 10)
	if !truncated {
		t.Fatal("expected truncation of long token")
	}
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != 10 {
		t.Errorf("expected hard cut at 10 runes, got %d: %q", n, got)
	}
}

func TestCompressRows_ZeroConfigIsNoOp(t *testing.T) {
	rows := []Row{
		{ID: "1", Kind: "stable_fact", Scope: "public", Title: "A", Content: "# long body"},
		{ID: "2", Kind: "event", Scope: "public", Title: "B", Content: "more"},
	}
	res := CompressRows(rows, Config{})
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(res.Rows))
	}
	if res.Rows[0].Content != "# long body" {
		t.Errorf("zero config must not alter content: %q", res.Rows[0].Content)
	}
	if res.Constant != nil {
		t.Errorf("zero config must not factor: %v", res.Constant)
	}
}

func TestCompressRows_FactorsConstantFields(t *testing.T) {
	rows := []Row{
		{ID: "1", Kind: "stable_fact", Scope: "public", Title: "A", Content: "x"},
		{ID: "2", Kind: "stable_fact", Scope: "public", Title: "B", Content: "y"},
		{ID: "3", Kind: "stable_fact", Scope: "private", Title: "C", Content: "z"},
	}
	res := CompressRows(rows, Config{MaxContentChars: 100})
	if res.Constant["kind"] != "stable_fact" {
		t.Errorf("uniform kind should be factored, got %v", res.Constant)
	}
	if _, ok := res.Constant["scope"]; ok {
		t.Errorf("non-uniform scope must not be factored, got %v", res.Constant)
	}
	for _, r := range res.Rows {
		if r.Kind != "" {
			t.Errorf("factored kind should be blanked on row %s: %q", r.ID, r.Kind)
		}
		if r.Scope == "" {
			t.Errorf("non-factored scope should remain on row %s", r.ID)
		}
	}
}

func TestCompressRows_LossyCompressSetsTruncatedFlag(t *testing.T) {
	long := strings.Repeat("word ", 200)
	rows := []Row{{ID: "1", Kind: "stable_fact", Title: "A", Content: long}}
	res := CompressRows(rows, Config{MaxContentChars: 50})
	if !res.Rows[0].Truncated {
		t.Error("expected Truncated=true for over-budget content")
	}
	if n := len([]rune(res.Rows[0].Content)); n > 60 {
		t.Errorf("content not capped near budget: %d runes", n)
	}
}
