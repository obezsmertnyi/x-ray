package mcpserver

// How long a note lives. Declared here rather than in engine for the same
// reason Note is: engine imports this package, and the server flag, the tool
// argument and the engine all read the one syntax.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TTLNever is Note.TTL's "keep forever" sentinel. Zero means "the server's
// default", so never needs a value of its own; any negative duration reads as
// never, and this is the one to write.
const TTLNever time.Duration = -1

// DefaultMemoryTTL is how long a note lives when neither the note nor the
// server says. A week: long enough that "what did we do last time" still
// works after a weekend and a few days off, short enough that a session's
// running notes do not pile up for months and rank beside this week's. A
// note worth longer says so (ttl=30d, ttl=never).
const DefaultMemoryTTL = 7 * 24 * time.Hour

// ParseTTL reads a lifetime: a Go duration ("24h", "90m"), a day count with a
// d suffix ("7d", "0.5d" — Go's parser has no day unit), or "never". Empty
// means "server default" and returns 0. Anything else, including zero and
// negative durations, is an error: a note that expires at once is a mistake,
// not a request.
func ParseTTL(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return 0, nil
	case "never", "forever":
		return TTLNever, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("ttl must be a duration like 7d, 24h or 90m, or never; got %q", s)
		}
		d = time.Duration(f * float64(24*time.Hour))
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("ttl must be a duration like 7d, 24h or 90m, or never; got %q", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("ttl must be positive, got %q", s)
	}
	return d, nil
}

// FormatTTL renders a lifetime the way ParseTTL reads it: "never" for the
// sentinel, whole days as "7d", anything else as a Go duration.
func FormatTTL(d time.Duration) string {
	switch {
	case d < 0:
		return "never"
	case d > 0 && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	default:
		return d.String()
	}
}
