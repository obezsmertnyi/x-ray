package mcpserver

import (
	"testing"
	"time"
)

func TestParseTTL(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", 0, false},
		{"never", TTLNever, false},
		{" Forever ", TTLNever, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"0.5d", 12 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"1s", time.Second, false},
		{"0", 0, true},
		{"0d", 0, true},
		{"-1h", 0, true},
		{"soon", 0, true},
		{"7 days", 0, true},
	}
	for _, c := range cases {
		got, err := ParseTTL(c.in)
		if (err != nil) != c.err {
			t.Errorf("ParseTTL(%q): err=%v, want err=%v", c.in, err, c.err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("ParseTTL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFormatTTL_RoundTrips(t *testing.T) {
	for in, want := range map[string]string{"7d": "7d", "1d": "1d", "36h": "36h0m0s", "90m": "1h30m0s", "never": "never"} {
		d, err := ParseTTL(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := FormatTTL(d); got != want {
			t.Errorf("FormatTTL(ParseTTL(%q)) = %q, want %q", in, got, want)
		}
	}
	if DefaultMemoryTTL != 7*24*time.Hour || FormatTTL(DefaultMemoryTTL) != "7d" {
		t.Fatalf("the default lifetime is a week and prints as 7d, got %s", FormatTTL(DefaultMemoryTTL))
	}
}
