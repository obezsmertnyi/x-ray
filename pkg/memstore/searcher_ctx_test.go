package memstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSearchCallerFromCtx_DefaultsToInternal proves an un-stamped context
// (every call site that predates WithSearchCaller, or a caller that forgets
// to stamp one) attributes to "internal" rather than panicking or reading as
// the empty string.
func TestSearchCallerFromCtx_DefaultsToInternal(t *testing.T) {
	assert.Equal(t, "internal", searchCallerFromCtx(context.Background()))
}

// TestSearchCallerFromCtx_ReturnsStampedCaller proves WithSearchCaller's
// stamp round-trips through searchCallerFromCtx.
func TestSearchCallerFromCtx_ReturnsStampedCaller(t *testing.T) {
	ctx := WithSearchCaller(context.Background(), "voice")
	assert.Equal(t, "voice", searchCallerFromCtx(ctx))
}
