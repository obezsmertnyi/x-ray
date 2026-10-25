package xray

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── event_time reader annotation (docs/todo/graphiti-comparison-refinement.md #1) ──

func TestEventTimeAnnotation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty eventDate produces no annotation", "", ""},
		{"unparseable eventDate produces no annotation", "not-a-date", ""},
		{"RFC3339 midnight renders as date only", "2023-05-30T00:00:00Z", " (event_time: 2023-05-30)"},
		{"RFC3339 with a time-of-day renders the full timestamp", "2026-06-15T09:00:00Z", " (event_time: 2026-06-15T09:00:00Z)"},
		{"bare YYYY-MM-DD renders as date only", "2023-05-30", " (event_time: 2023-05-30)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, eventTimeAnnotation(c.in))
		})
	}
}

func TestParseEventDate(t *testing.T) {
	_, ok := parseEventDate("")
	assert.False(t, ok, "empty string must not parse")

	_, ok = parseEventDate("garbage")
	assert.False(t, ok, "unparseable input must not parse")

	_, ok = parseEventDate("2026-06-15T09:00:00Z")
	assert.True(t, ok, "RFC3339 must parse")

	_, ok = parseEventDate("2026-06-15")
	assert.True(t, ok, "bare YYYY-MM-DD must parse")
}

func TestAnnotateEventTimes(t *testing.T) {
	withDate := &MemoryItem{ID: "a", Title: "Deploy widget-api", EventDate: "2023-05-30T00:00:00Z"}
	withoutDate := &MemoryItem{ID: "b", Title: "widget-api owns billing"}

	out := annotateEventTimes([]*MemoryItem{withDate, withoutDate})
	require.Len(t, out, 2, "must return one row per input item")

	assert.Equal(t, "Deploy widget-api (event_time: 2023-05-30)", out[0].Title)
	assert.Equal(t, "widget-api owns billing", out[1].Title)

	// The original items must never be mutated — annotateEventTimes clones
	// before appending the annotation, since Search/GetItem can return a
	// pointer aliasing live store state.
	assert.Equal(t, "Deploy widget-api", withDate.Title, "source item title must be untouched")
}
