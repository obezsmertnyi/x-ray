package parser

import (
	"embed"
	"fmt"
)

// Vendored Tree-sitter `tags.scm` queries, copied from each grammar's queries/
// directory. They drive the generic, per-language symbol+reference extraction
// (Tier 1) so any supported language is searchable without a hand-written
// extractor. Rich structural extractors (e.g. Go) override this per language.
//
//go:embed queries/*.scm
var queryFS embed.FS

// tagsQuery returns the embedded tags query source for a language name, or ""
// if none is vendored.
func tagsQuery(lang string) string {
	b, err := queryFS.ReadFile(fmt.Sprintf("queries/%s.scm", lang))
	if err != nil {
		return ""
	}
	return string(b)
}
