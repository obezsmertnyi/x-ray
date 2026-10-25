package engine

import (
	"bytes"
	"strings"
	"testing"
)

// bundle builds source that is `lines` lines long and `size` bytes total, to
// exercise the mean-line-length heuristic at a chosen density.
func bundle(size, lines int) []byte {
	if lines < 1 {
		lines = 1
	}
	b := bytes.Repeat([]byte("x"), size)
	for i := 1; i < lines; i++ {
		b[i*(size/lines)] = '\n'
	}
	return b
}

func TestIsMinified_ByName(t *testing.T) {
	// Small bodies throughout: these must be caught by name alone, since the
	// content heuristic never fires below minifiedMinBytes.
	small := []byte("var a = 1;\n")
	for _, name := range []string{
		"assets/d3.v7.min.js",
		"web/app.min.css",
		"vendor-min.js",
		"static/bundle.min.mjs",
		"lib/thing.min.ts",
		"app.js.map",
		"DEEP/PATH/JQUERY.MIN.JS", // matching is case-insensitive
	} {
		if !IsMinified(name, small) {
			t.Errorf("IsMinified(%q) = false, want true", name)
		}
	}
}

func TestIsMinified_LeavesRealSourceAlone(t *testing.T) {
	// Ordinary code, including files whose names merely contain "min".
	code := []byte(strings.Repeat("func doSomething() error { return nil }\n", 4000))
	for _, name := range []string{
		"internal/engine/parse.go",
		"web/src/admin.js",   // "min" inside a word
		"web/src/mineral.ts", // ditto
		"scripts/minify.js",  // a minifier is not itself minified
		"internal/ui/index.html",
	} {
		if IsMinified(name, code) {
			t.Errorf("IsMinified(%q) = true, want false", name)
		}
	}
}

func TestIsMinified_ByContent_CatchesUnconventionallyNamedBundles(t *testing.T) {
	// A webpack-style hashed bundle: no ".min" in the name, but 200 KB on 3
	// lines. This is the case a name list alone cannot catch.
	src := bundle(200<<10, 3)
	if !IsMinified("web/static/main.a1b2c3d4.js", src) {
		t.Fatalf("a 200KB 3-line bundle must be detected as minified")
	}
}

func TestIsMinified_ByContent_RequiresBothSizeAndDensity(t *testing.T) {
	// Dense but small: below minifiedMinBytes, so parsing it is cheap and it is
	// probably hand-written. Must NOT be skipped.
	if IsMinified("web/small.js", bundle(10<<10, 2)) {
		t.Errorf("a 10KB file must not be treated as minified on density alone")
	}
	// Large but normally wrapped: 200 KB across 5000 lines (~40 bytes/line).
	if IsMinified("web/big.js", bundle(200<<10, 5000)) {
		t.Errorf("a large, normally-wrapped file must not be treated as minified")
	}
}

func TestIsMinified_NeverDropsDocsOrManifests(t *testing.T) {
	// An unwrapped Markdown doc (one long line per paragraph) can be large and
	// dense. Prose is the payload for search_graph_docs, so the content
	// heuristic must not apply to it — nor to YAML manifests.
	dense := bundle(200<<10, 3)
	if IsMinified("docs/ARCHITECTURE.md", dense) {
		t.Errorf("a large unwrapped Markdown doc must never be skipped")
	}
	if IsMinified("charts/app/templates/deployment.yaml", dense) {
		t.Errorf("a large YAML manifest must never be skipped")
	}
}
