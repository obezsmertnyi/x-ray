package xray

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ── trigrams ──────────────────────────────────────────────────────────────────

func TestTrigrams_Short(t *testing.T) {
	// strings shorter than 3 chars are returned as-is
	assert.Equal(t, []string{"re"}, trigrams("re"))
	assert.Equal(t, []string{"r"}, trigrams("r"))
	assert.Equal(t, []string{"rec"}, trigrams("rec"))
}

// ── trigramIndex ──────────────────────────────────────────────────────────────

// ── KeywordSearch with trigram index ─────────────────────────────────────────
