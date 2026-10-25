package index

import "testing"

func TestNormalizeQuerySplitsCamelAndSeparators(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"build-cli", "build cli"},
		{"buildCLI", "build cli"}, // splits at the lower→upper boundary, then lowercases
		{"makeBuildCli", "make build cli"},
		{"snake_case_name", "snake case name"},
		{"Already Lower", "already lower"},
	} {
		if got := normalizeQuery(tc.in); got != tc.want {
			t.Errorf("normalizeQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestKwTokensDropsStopwordsAndSingleChars(t *testing.T) {
	// The question form is mostly stopwords; what must survive is the content.
	got := kwTokens(normalizeQuery("I edited the jucce cli source, what do I need to run?"))
	want := map[string]bool{"edited": true, "jucce": true, "cli": true, "source": true}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want exactly %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected token %q in %v", g, got)
		}
	}
}

func TestKwTokensAllStopwordsAbstains(t *testing.T) {
	if got := kwTokens(normalizeQuery("what should I do about it?")); len(got) != 0 {
		t.Errorf("expected no content tokens, got %v", got)
	}
}

func TestTrigramCandidatesRequireEveryToken(t *testing.T) {
	idx := newTrigramIndex()
	idx.index("a", "jucce cli build", "after changing cli/jucce run make build-cli")
	idx.index("b", "jucce identity", "jucce voice session architecture and skills")
	idx.index("c", "helm upgrade", "never upgrade without reuse-values")

	// Both jucce docs contain "jucce"; only "a" also contains "cli".
	cands, _ := idx.candidates(tokenGrams([]string{"jucce", "cli"}))
	if _, ok := cands["a"]; !ok {
		t.Errorf("expected 'a' to match both tokens, got %v", cands)
	}
	if _, ok := cands["b"]; ok {
		t.Errorf("'b' has no 'cli' and must not match: %v", cands)
	}
	if _, ok := cands["c"]; ok {
		t.Errorf("'c' is unrelated and must not match: %v", cands)
	}
}

func TestTrigramShortTokenNeedsExactMatch(t *testing.T) {
	idx := newTrigramIndex()
	idx.index("a", "cli tool", "command line")
	// "cli" is 1 trigram → threshold 1.0, so a near word must not match.
	if c, _ := idx.candidates(tokenGrams([]string{"clx"})); len(c) != 0 {
		t.Errorf("short token must match exactly, got %v", c)
	}
	if c, _ := idx.candidates(tokenGrams([]string{"cli"})); len(c) != 1 {
		t.Errorf("exact short token should match, got %v", c)
	}
}

func TestTrigramLongTokenRejectsNearMiss(t *testing.T) {
	idx := newTrigramIndex()
	idx.index("a", "goroutine", "a goroutine leaks")
	// Long tokens use a 0.90 threshold — "coroutines" must not match "goroutine".
	if c, _ := idx.candidates(tokenGrams([]string{"coroutines"})); len(c) != 0 {
		t.Errorf("long near-miss must not match, got %v", c)
	}
}

func TestIndexReplacesPostingsForSameID(t *testing.T) {
	idx := newTrigramIndex()
	idx.index("a", "original", "vllm macos build")
	idx.index("a", "rewritten", "helm upgrade overlay")
	if c, _ := idx.candidates(tokenGrams([]string{"vllm"})); len(c) != 0 {
		t.Errorf("stale postings survived re-index: %v", c)
	}
	if c, _ := idx.candidates(tokenGrams([]string{"overlay"})); len(c) != 1 {
		t.Errorf("re-indexed content not searchable: %v", c)
	}
}

func TestLexScorePrefersTitleThenCoverage(t *testing.T) {
	toks := []string{"helm", "overlay"}
	q := "helm overlay"
	exact := lexScore("helm overlay", "body", q, toks, 2, 1)
	contains := lexScore("helm overlay drift", "body", q, toks, 2, 1)
	contentOnly := lexScore("unrelated", "helm overlay in the body", q, toks, 2, 1)
	if !(exact > contains && contains > contentOnly) {
		t.Errorf("expected exact(%d) > contains(%d) > contentOnly(%d)", exact, contains, contentOnly)
	}
}

func TestFuseRRFRewardsAgreementAndKeepsCosine(t *testing.T) {
	sem := []Hit{{QN: "x", Score: 0.7}, {QN: "y", Score: 0.6}}
	lex := []Hit{{QN: "y", Score: 0}, {QN: "z", Score: 0}}

	out, promotions, semanticOnly, both := fuseRRF(nil, lex, sem, 3)
	if len(out) != 3 {
		t.Fatalf("want 3 fused hits, got %d", len(out))
	}
	// y appears in both lists, so it must outrank x (semantic-only, rank 0).
	if out[0].QN != "y" {
		t.Errorf("expected 'y' first (found by both channels), got %q", out[0].QN)
	}
	// The semantic Hit must win the slot so callers still see a real cosine.
	if out[0].Score != 0.6 {
		t.Errorf("expected y to keep its cosine 0.6, got %v", out[0].Score)
	}
	// x: semantic-only. y: both. z: keyword-only (a promotion — sem never
	// ranked it at all).
	if promotions != 1 || semanticOnly != 1 || both != 1 {
		t.Errorf("expected 1 promotion, 1 semantic-only, 1 both; got %d/%d/%d", promotions, semanticOnly, both)
	}
	if promotions+semanticOnly+both != len(out) {
		t.Errorf("partition counts %d+%d+%d must sum to len(out)=%d", promotions, semanticOnly, both, len(out))
	}
}

func TestFuseRRFLimit(t *testing.T) {
	sem := []Hit{{QN: "a"}, {QN: "b"}, {QN: "c"}}
	out, promotions, semanticOnly, both := fuseRRF(nil, nil, sem, 2)
	if len(out) != 2 {
		t.Errorf("limit not applied: got %d", len(out))
	}
	if promotions != 0 || semanticOnly != 2 || both != 0 {
		t.Errorf("expected 0/2/0 (semantic-only only, truncated to limit), got %d/%d/%d", promotions, semanticOnly, both)
	}
}

func TestCandidatesReportsRejectedCount(t *testing.T) {
	idx := newTrigramIndex()
	// "full" matches every token. "weak" shares a trigram via "code" (so it
	// clears the posting-list SEED step) but only matches 1 of 3 tokens —
	// minCoverage(3) is 2, so it must be rejected, and that rejection must be
	// counted, not silently dropped along with the candidate itself.
	idx.index("full", "full match", "supposed code project")
	idx.index("weak", "weak match", "code only, nothing else relevant")
	toks := []string{"supposed", "code", "project"}

	cands, rejected := idx.candidates(tokenGrams(toks))
	if _, ok := cands["full"]; !ok {
		t.Fatalf("full-coverage doc must be a candidate: %v", cands)
	}
	if _, ok := cands["weak"]; ok {
		t.Fatalf("under-coverage doc must not be a candidate: %v", cands)
	}
	if rejected != 1 {
		t.Errorf("expected exactly 1 rejected candidate (weak), got %d", rejected)
	}
}

func TestMinCoverageToleratesIncidentalQueryWords(t *testing.T) {
	// Short queries must match fully; longer question-form queries may miss a
	// third of their tokens, which is what lets a real question find its answer.
	for _, tc := range []struct{ n, want int }{{1, 1}, {2, 2}, {3, 2}, {4, 3}, {5, 4}, {6, 4}} {
		if got := minCoverage(tc.n); got != tc.want {
			t.Errorf("minCoverage(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestCandidatesAllowPartialCoverageButScorePenalisesIt(t *testing.T) {
	idx := newTrigramIndex()
	// "full" has every token; "partial" is missing "implementation".
	idx.index("full", "subagents", "who writes implementation code on this project")
	idx.index("partial", "other", "code on this project")
	toks := []string{"supposed", "write", "implementation", "code", "project"}

	cands, _ := idx.candidates(tokenGrams(toks))
	if _, ok := cands["full"]; !ok {
		t.Fatalf("full-coverage doc must be a candidate despite the absent 'supposed': %v", cands)
	}

	full := lexScore("subagents", "who writes implementation code on this project",
		"", toks, cands["full"].matched, cands["full"].frac)
	if c, ok := cands["partial"]; ok {
		partial := lexScore("other", "code on this project", "", toks, c.matched, c.frac)
		if full <= partial {
			t.Errorf("fuller coverage must rank higher: full=%d partial=%d", full, partial)
		}
	}
}
