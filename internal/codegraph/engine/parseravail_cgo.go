//go:build cgo

package engine

// parserAvailable reports whether this build carries the Tree-sitter code
// extractor. Used by the incremental update path to decide whether a code file
// can be re-indexed or must be skipped.
const parserAvailable = true
