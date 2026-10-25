//go:build !cgo

package engine

// parserAvailable is false in the CGO-free build: parser.ParseFile is a stub
// that yields no nodes. Doc-shaped files (Markdown, YAML) are still fully
// indexable — their extractors are pure Go.
const parserAvailable = false
