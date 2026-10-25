package engine

// Per-file dispatch, shared by the CGO and CGO-free builds.
//
// Deliberately UNTAGGED. Only the Tree-sitter *code* extractor needs CGO;
// Markdown, Kubernetes YAML and SQL scripts are handled by pure-Go extractors
// (docs.Parse / k8s.Parse / sqlddl.Parse) that this dispatcher reaches BEFORE
// the parser is ever consulted.
// Keeping the dispatch behind the `cgo` tag is what used to force the whole
// incremental-update path — including doc-only corpora — to require a C
// toolchain it never actually used.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/appconfig"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/compose"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/dockerfile"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/docs"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/k8s"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/sqlddl"
)

// isMarkdown reports whether a path is a Markdown doc.
func isMarkdown(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

// isYAML reports whether a path is a YAML file (treated as K8s manifests).
func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

// isSQL reports whether a path is a SQL script (schema objects, see sqlddl).
func isSQL(path string) bool {
	return strings.ToLower(filepath.Ext(path)) == ".sql"
}

// NeedsCodeParser reports whether extracting this file requires the Tree-sitter
// parser — i.e. whether it is not Markdown, YAML, SQL, an .env file, an
// application config file or a Dockerfile. In a CGO-free build
// such a file cannot be re-indexed, and callers must SKIP it rather than parse
// it: ParseOneFile would return zero nodes there, which the incremental update
// path is entitled to read as "the file was deleted" and would use to drop the
// file's existing nodes from a worked copy. Silent data loss, from a build flag.
func NeedsCodeParser(rel string) bool {
	return !isMarkdown(rel) && !isYAML(rel) && !isSQL(rel) && !compose.IsDotEnv(rel) &&
		!appconfig.IsConfigFile(rel) && !dockerfile.IsDockerfile(rel)
}

// A file is treated as a minified bundle when it is at least minifiedMinBytes
// long AND averages at least minifiedMeanLineLen bytes per line. The gap these
// sit in is wide: the largest hand-written sources in a typical repo average
// 30-50 bytes per line, while a bundler's output averages tens of thousands.
const (
	minifiedMinBytes    = 50 << 10
	minifiedMeanLineLen = 1000
)

// minifiedExts are the extensions a ".min"/"-min" infix marks as pre-built.
var minifiedExts = []string{".js", ".mjs", ".cjs", ".css", ".ts"}

// IsMinified reports whether a file is a minified or generated bundle that
// should not be indexed.
//
// skipDir already excludes vendor/, node_modules/ and dist/, but a bundle
// committed into an ordinary directory slips straight through, and the cost is
// severe: one vendored d3.v7.min.js parses into ~2000 single-letter "functions"
// — 77% of every node in this repo's own graph before this check existed. Those
// nodes swamp real symbols in search ranking, and each one is embedded and
// carried in every snapshot.
//
// Two signals, because neither alone is enough. The name check catches the
// conventional cases cheaply. The content check catches a bundle named anything
// at all (webpack's main.<hash>.js), and is what makes this robust rather than
// a list of spellings; it is confined to files that need the code parser, so an
// unwrapped Markdown doc with very long lines is never dropped.
func IsMinified(rel string, src []byte) bool {
	base := strings.ToLower(filepath.Base(rel))
	if strings.HasSuffix(base, ".map") { // source maps: JSON, never useful here
		return true
	}
	for _, ext := range minifiedExts {
		if strings.HasSuffix(base, ".min"+ext) || strings.HasSuffix(base, "-min"+ext) {
			return true
		}
	}
	if !NeedsCodeParser(rel) || len(src) < minifiedMinBytes {
		return false
	}
	return len(src)/(bytes.Count(src, []byte{'\n'})+1) >= minifiedMeanLineLen
}

// ParseOneFile dispatches a single file's bytes to the right extractor:
// Markdown → Doc/Section nodes, K8s YAML → Manifest nodes, SQL → schema-object
// nodes (Table, View, Routine, Trigger, Sequence), else Tree-sitter
// code. rel is the repo-relative path. Shared by full and incremental indexing.
//
// In a CGO-free build the default branch reaches the parser stub and yields no
// nodes; callers that cannot tolerate that must gate on NeedsCodeParser first.
func ParseOneFile(p *parser.Parser, rel string, src []byte) parser.Result {
	switch {
	case isMarkdown(rel):
		return docs.Parse(rel, src)
	case appconfig.IsConfigFile(rel):
		return appconfig.Parse(rel, src)
	case dockerfile.IsDockerfile(rel):
		return dockerfile.Parse(rel, src)
	case isYAML(rel) && compose.IsComposeFile(src):
		return compose.Parse(rel, src)
	case isYAML(rel):
		return k8s.Parse(rel, src)
	case compose.IsDotEnv(rel):
		return compose.ParseDotEnv(rel, src)
	case isSQL(rel):
		return sqlddl.Parse(rel, src)
	default:
		res, err := p.ParseFile(rel, src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", rel, err)
			return parser.Result{}
		}
		return res
	}
}
