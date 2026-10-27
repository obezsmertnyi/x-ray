package xray

import (
	"context"

	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
)

// This file is the surface the public library (pkg/memory) builds on. It keeps
// the rules the MCP server applies at its boundary (what may be written, who a
// write is attributed to) reachable by a program that embeds the store.

// ValidateWrite checks what an external writer may put in an item: a kind from
// the vocabulary, and high confidence on a stable_fact. "" means valid.
func ValidateWrite(kind, confidence string) string {
	return memstore.BaseVocabulary().ValidateWrite(kind, confidence)
}

// ValidateSearchKind rejects a kind filter that names no searchable kind. An
// empty filter means every kind and is valid.
func ValidateSearchKind(kind string) string {
	return memstore.BaseVocabulary().ValidateSearchKind(kind)
}

// WithOrigin returns ctx carrying the provenance of the caller. Upsert reads it:
// a write made as OriginHuman is never replaced by a derived one.
func WithOrigin(ctx context.Context, origin string) context.Context {
	return withWriteOrigin(ctx, origin)
}
