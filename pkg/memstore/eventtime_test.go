package memstore

import (
	"testing"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// ── event_time reader annotation ──

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
			if got := EventTimeAnnotation(c.in); got != c.want {
				t.Errorf("EventTimeAnnotation(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseEventDate(t *testing.T) {
	if _, ok := ParseEventDate(""); ok {
		t.Error("empty string must not parse")
	}
	if _, ok := ParseEventDate("garbage"); ok {
		t.Error("unparseable input must not parse")
	}
	if _, ok := ParseEventDate("2026-06-15T09:00:00Z"); !ok {
		t.Error("RFC3339 must parse")
	}
	if _, ok := ParseEventDate("2026-06-15"); !ok {
		t.Error("bare YYYY-MM-DD must parse")
	}
}

// TestMemoryItem_TitleWithEventTime proves the composition helper every
// render call site uses: annotation appended when EventDate is present and
// parseable, Title returned unchanged otherwise.
func TestMemoryItem_TitleWithEventTime(t *testing.T) {
	withDate := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		ID: "a", Title: "Deploy sample-api", EventDate: "2023-05-30T00:00:00Z",
	}}
	withoutDate := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		ID: "b", Title: "sample-api owns billing",
	}}
	badDate := Item{MemoryCellSpec: memoryv1.MemoryCellSpec{
		ID: "c", Title: "legacy cell", EventDate: "not-a-date",
	}}

	if got, want := withDate.TitleWithEventTime(), "Deploy sample-api (event_time: 2023-05-30)"; got != want {
		t.Errorf("TitleWithEventTime() = %q, want %q", got, want)
	}
	if got, want := withoutDate.TitleWithEventTime(), "sample-api owns billing"; got != want {
		t.Errorf("TitleWithEventTime() = %q, want %q", got, want)
	}
	if got, want := badDate.TitleWithEventTime(), "legacy cell"; got != want {
		t.Errorf("TitleWithEventTime() with an unparseable EventDate = %q, want %q (degrade to no annotation)", got, want)
	}
}
