package engine

import (
	"testing"

	"github.com/rs/zerolog"
)

// newLabelRegistry builds a registry holding nothing but labels. The routing
// helpers under test read labels only — no embedder, no snapshot body — so this
// file stays untagged and runs in both build modes.
func newLabelRegistry(labels ...string) *Registry {
	r := NewRegistry(nil, EmbedFingerprint{}, 3, 0, false, zerolog.Nop())
	for _, l := range labels {
		r.entries[l] = &projectEntry{label: l}
	}
	return r
}

func TestEntryMatchesLabelCaseInsensitively(t *testing.T) {
	r := newLabelRegistry("academy", "mcp-security-governance")

	for _, in := range []string{"academy", "Academy", "ACADEMY", "AcAdEmY"} {
		e := r.entry(in)
		if e == nil || e.label != "academy" {
			t.Fatalf("entry(%q) = %v, want the academy entry", in, e)
		}
	}
	if e := r.entry("MCP-Security-Governance"); e == nil || e.label != "mcp-security-governance" {
		t.Fatalf("hyphenated label must match case-insensitively, got %v", e)
	}
	if e := r.entry("nosuchmap"); e != nil {
		t.Fatalf("entry of an unregistered label must be nil, got %v", e)
	}
}

func TestLabelsNamedInMatchesWholeWordsOnly(t *testing.T) {
	r := newLabelRegistry("academy", "ville", "alpha", "beta")

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"label named in prose", "tools and services used by Jane Roe in project Academy", []string{"academy"}},
		{"trailing punctuation is a boundary", "look in project academy.", []string{"academy"}},
		{"suffix is not a match", "who academies the load", nil},
		{"prefix is not a match", "the village layout", nil},
		{"ordered by first appearance", "compare alpha with beta", []string{"alpha", "beta"}},
		{"order follows the text, not the map", "compare beta with alpha", []string{"beta", "alpha"}},
		{"no label named", "how does eviction work", nil},
		{"empty text", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.labelsNamedIn(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("labelsNamedIn(%q) returned %d maps, want %d (%v)", tc.text, len(got), len(tc.want), labelsOf(got))
			}
			for i, w := range tc.want {
				if got[i].label != w {
					t.Fatalf("labelsNamedIn(%q) = %v, want %v", tc.text, labelsOf(got), tc.want)
				}
			}
		})
	}
}

func TestWordIndexReportsFirstWholeWordPosition(t *testing.T) {
	cases := []struct {
		s, word string
		want    int
	}{
		{"in project academy", "academy", 11},
		{"academy first, academy again", "academy", 0},
		{"academies then academy", "academy", 15},
		{"academies", "academy", -1},
		{"a-b-c", "a-b-c", 0},
		{"xa-b-cx", "a-b-c", -1},
		{"anything", "", -1},
	}
	for _, tc := range cases {
		if got := wordIndex(tc.s, tc.word); got != tc.want {
			t.Errorf("wordIndex(%q, %q) = %d, want %d", tc.s, tc.word, got, tc.want)
		}
	}
}

func labelsOf(es []*projectEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.label
	}
	return out
}
