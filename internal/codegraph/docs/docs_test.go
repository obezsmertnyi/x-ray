package docs

import (
	"strings"
	"testing"
)

func TestParse_NoDuplicateOverview_WhenPreambleIsFrontMatter(t *testing.T) {
	src := []byte("# ADR-000: Architecture Overview\n\n**Status:** Reference\n**Date:** 2026-06-06\n\n---\n\n## Overview\n\nRecall is a thing.\n\n## Decision\n\nUse HNSW.\n")
	res := Parse("docs/adr/000-Arch.md", src)
	// One Doc + exactly the H2 sections (Overview, Decision); NO duplicate Overview,
	// NO "(preamble)" section since the preamble is only title+status+---.
	var sections []string
	docs := 0
	for _, n := range res.Nodes {
		switch n.Kind {
		case KindDoc:
			docs++
		case KindSection:
			sections = append(sections, n.Name)
		}
	}
	if docs != 1 {
		t.Fatalf("want 1 Doc node, got %d", docs)
	}
	want := []string{"Overview", "Decision"}
	if len(sections) != len(want) {
		t.Fatalf("want sections %v, got %v", want, sections)
	}
	for i, w := range want {
		if sections[i] != w {
			t.Fatalf("section[%d] = %q, want %q (full: %v)", i, sections[i], w, sections)
		}
	}
}

func TestParse_KeepsPreamble_WhenItHasProse(t *testing.T) {
	src := []byte("# Title\n\nA real intro paragraph before any heading.\n\n## Section A\n\nbody\n")
	res := Parse("docs/x.md", src)
	hasPreamble := false
	for _, n := range res.Nodes {
		if n.Kind == KindSection && n.Name == preambleTitle {
			hasPreamble = true
		}
	}
	if !hasPreamble {
		t.Fatalf("expected a %q section when preamble has prose", preambleTitle)
	}
}

// A "# " line inside a fenced code block is a shell comment, not a heading.
// Treating it as one silently mis-titled a real memory file and made it
// describe a line of code — see docs/todo/cc-memory-eval.md.
func TestFirstTitleIgnoresFencedHeadings(t *testing.T) {
	src := []byte("---\nname: x\n---\n\nSome prose about building.\n\n```\npip install foo\n# build backend deps must be installed manually\npip install bar\n```\n\nMore prose.\n")
	if got := firstTitle(src, "notes/build.md"); got != "notes/build.md" {
		t.Errorf("fenced comment leaked as title: got %q, want the path fallback", got)
	}
}

func TestFirstTitleStillFindsRealHeading(t *testing.T) {
	src := []byte("# Real Title\n\n```\n# not a title\n```\n")
	if got := firstTitle(src, "x.md"); got != "Real Title" {
		t.Errorf("got %q, want %q", got, "Real Title")
	}
}

// A real H1 appearing after a fenced block must still be found — the fence
// toggle has to close, not swallow the rest of the document.
func TestFirstTitleAfterFencedBlock(t *testing.T) {
	src := []byte("```\n# fake\n```\n\n# Actual Title\n")
	if got := firstTitle(src, "x.md"); got != "Actual Title" {
		t.Errorf("got %q, want %q", got, "Actual Title")
	}
}

func TestSectionsIgnoreFencedHeadings(t *testing.T) {
	src := []byte("# Doc\n\nIntro prose that is long enough to count as real prose here.\n\n## Real Section\n\nbody\n\n```md\n## Not A Section\n```\n\nmore body\n\n## Second Real\n\nbody\n")
	got := sections(src)
	var titles []string
	for _, s := range got {
		titles = append(titles, s.title)
	}
	for _, tt := range titles {
		if tt == "Not A Section" {
			t.Fatalf("fenced heading became a section: %v", titles)
		}
	}
	var real int
	for _, tt := range titles {
		if tt == "Real Section" || tt == "Second Real" {
			real++
		}
	}
	if real != 2 {
		t.Errorf("expected both real sections, got %v", titles)
	}
}

// The fenced "## " must stay in its section's body — ignoring it as a heading
// must not also delete it from the text.
func TestFencedHeadingStaysInBody(t *testing.T) {
	src := []byte("# Doc\n\nIntro prose that is long enough to count as real prose here.\n\n## Real Section\n\n```md\n## Not A Section\n```\n")
	for _, s := range sections(src) {
		if s.title == "Real Section" {
			if !strings.Contains(s.body, "## Not A Section") {
				t.Errorf("fenced heading dropped from body: %q", s.body)
			}
			return
		}
	}
	t.Fatal("Real Section not found")
}
