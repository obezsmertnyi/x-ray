package memstore

import "time"

// eventDateLayouts are the formats an Item.EventDate is accepted in, matched in
// order: RFC3339 (the field's documented format), then a bare calendar date.
var eventDateLayouts = []string{time.RFC3339, "2006-01-02"}

// ParseEventDate parses s against eventDateLayouts, reporting whether it
// parsed. Empty input never parses; callers check for "" separately, since "no
// event date" (omit the annotation silently) and "bad event date" (skip it)
// need different handling.
func ParseEventDate(s string) (time.Time, bool) {
	for _, layout := range eventDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ValidEventDate reports whether s is an event date the store can render back.
func ValidEventDate(s string) bool {
	_, ok := ParseEventDate(s)
	return ok
}

// EventTimeAnnotation renders a compact suffix for an item's EventDate, so a
// reader consuming search results as a model's context sees when a remembered
// item happened without parsing a separate field. Models answer temporal
// questions measurably worse when the dates are absent from the render.
// It returns "" when eventDate is empty or fails to parse under either accepted
// layout, so a bad or legacy value degrades to no annotation rather than
// garbage.
//
// It renders the date part only ("2023-05-30") when the parsed time is exactly
// UTC midnight, the shape a bare YYYY-MM-DD write produces, and the full
// RFC3339 timestamp otherwise.
//
// Append it to the Title, never the Content: a caller that truncates Content to
// fit a prompt would cut the annotation off the tail.
func EventTimeAnnotation(eventDate string) string {
	if eventDate == "" {
		return ""
	}
	t, ok := ParseEventDate(eventDate)
	if !ok {
		return ""
	}
	t = t.UTC()
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return " (event_time: " + t.Format("2006-01-02") + ")"
	}
	return " (event_time: " + t.Format(time.RFC3339) + ")"
}

// TitleWithEventTime returns the item's Title with its EventTimeAnnotation
// appended, if any.
func (i Item) TitleWithEventTime() string {
	return i.Title + EventTimeAnnotation(i.EventDate)
}
