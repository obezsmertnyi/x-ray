//go:build cgo

package parser

import (
	"path/filepath"
	"strings"
	"unsafe"

	tsswift "github.com/alex-pinkus/tree-sitter-swift/bindings/go"
	tskotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tscsharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tsc "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tscpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsphp "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tspython "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// langSpec describes how to extract structure for one language.
//
// Two tiers:
//   - Rich extractor (useTags == false): a hand-written cursor walk using the
//     node-type sets below + per-language helpers. Highest fidelity (resolved
//     call/import/inherit edges). Currently: Go.
//   - Generic tags (useTags == true): runs the grammar's vendored tags.scm to
//     emit symbol nodes + bare call edges for the linker. Zero per-language Go
//     code; covers every grammar that ships a tags query.
type langSpec struct {
	name     string
	language func() *tree_sitter.Language
	useTags  bool

	// Rich-extractor node-type sets (unused when useTags is true).
	classTypes  map[string]bool
	funcTypes   map[string]bool
	importTypes map[string]bool
	callTypes   map[string]bool
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

func lang(p unsafe.Pointer) func() *tree_sitter.Language {
	return func() *tree_sitter.Language { return tree_sitter.NewLanguage(p) }
}

// registry maps a language name to its spec. Go uses the rich extractor; every
// other language uses the generic tags-based extractor (Tier 1).
var registry = map[string]*langSpec{
	"go": {
		name:        "go",
		language:    func() *tree_sitter.Language { return tree_sitter.NewLanguage(tsgo.Language()) },
		classTypes:  set("type_declaration"),
		funcTypes:   set("function_declaration", "method_declaration"),
		importTypes: set("import_declaration"),
		callTypes:   set("call_expression"),
	},
	"python":     tagsSpec("python", tspython.Language()),
	"javascript": tagsSpec("javascript", tsjs.Language()),
	"typescript": tagsSpec("typescript", tsts.LanguageTypescript()),
	"tsx":        tagsSpec("typescript", tsts.LanguageTSX()), // tsx reuses ts tags
	"java":       tagsSpec("java", tsjava.Language()),
	"rust":       tagsSpec("rust", tsrust.Language()),
	"ruby":       tagsSpec("ruby", tsruby.Language()),
	"c":          tagsSpec("c", tsc.Language()),
	"cpp":        tagsSpec("cpp", tscpp.Language()),
	"php":        tagsSpec("php", tsphp.LanguagePHP()),
	"csharp":     tagsSpec("csharp", tscsharp.Language()),
	"kotlin":     tagsSpec("kotlin", tskotlin.Language()),
	"swift":      tagsSpec("swift", tsswift.Language()),
}

// tagsSpec builds a generic tags-based langSpec. queryName selects which
// vendored queries/<name>.scm to use (e.g. tsx reuses "typescript").
func tagsSpec(queryName string, ptr unsafe.Pointer) *langSpec {
	return &langSpec{
		name:     queryName,
		language: lang(ptr),
		useTags:  true,
	}
}

// extToLang resolves a file extension to a registered language name.
var extToLang = map[string]string{
	".go":    "go",
	".py":    "python",
	".js":    "javascript",
	".jsx":   "javascript",
	".mjs":   "javascript",
	".cjs":   "javascript",
	".ts":    "typescript",
	".tsx":   "tsx",
	".java":  "java",
	".rs":    "rust",
	".rb":    "ruby",
	".c":     "c",
	".h":     "c",
	".cc":    "cpp",
	".cpp":   "cpp",
	".cxx":   "cpp",
	".hpp":   "cpp",
	".hh":    "cpp",
	".php":   "php",
	".cs":    "csharp",
	".kt":    "kotlin",
	".kts":   "kotlin",
	".swift": "swift",
}

// detectLanguage returns the registered language for a path, or "" if unknown.
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if name, ok := extToLang[ext]; ok {
		if _, registered := registry[name]; registered {
			return name
		}
	}
	return ""
}
