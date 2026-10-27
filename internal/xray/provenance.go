package xray

import (
	"context"

	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
)

// Origin values stored in MemoryCellSpec.Origin: who a cell came from.
const (
	// OriginObserved: written by code from facts.
	OriginObserved = memstore.OriginObserved
	// OriginDerived: written by a model. A prior claim, never evidence. A cell
	// with no origin is derived.
	OriginDerived = memstore.OriginDerived
	// OriginHuman: written by an identified person. Outranks derived.
	OriginHuman = memstore.OriginHuman
	// OriginVerified: derived, then passed a check. Set only in code.
	OriginVerified = memstore.OriginVerified
)

// errDerivedReplacesHuman is the message for a derived write onto a human cell.
const errDerivedReplacesHuman = "a derived write cannot replace a human-authored cell"

func effectiveOrigin(o string) string {
	if o == "" {
		return OriginDerived
	}
	return o
}

type writeOriginKey struct{}

// withWriteOrigin stamps the provenance of the caller onto ctx; Upsert reads it.
func withWriteOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, writeOriginKey{}, origin)
}

func writeOriginFrom(ctx context.Context) string {
	o, _ := ctx.Value(writeOriginKey{}).(string)
	return o
}
