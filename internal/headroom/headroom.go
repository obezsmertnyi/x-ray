// Package headroom reduces the token cost of voice search_memory tool results
// before they are injected into the Gemini Live context window. It is a native
// Go port of the transferable ideas in https://github.com/chopratejas/headroom
// — see docs/adr/038-headroom-token-compression.md.
//
// Two mechanisms, both deterministic (no model call):
//
//  1. LossyCompress — markdown-noise-aware truncation of a long Content body to
//     a character budget, on a word boundary. The full body stays retrievable
//     by ID (the existing item-fetch path is the reversible "fetch original"
//     contract, so no separate cache is needed).
//
//  2. Factor — hoist fields that are constant across every row out of the
//     per-row JSON into a shared header, so they are paid for once.
//
// Both are opt-in: with a zero/empty Config the input passes through unchanged.
//
// Headroom's third mechanism — AdaptiveK (Kneedle/SimHash near-duplicate row
// collapse) — was ported, tested live, and removed: on the X-Ray corpus it
// omitted zero rows (cells are genuinely distinct), and dropping result rows
// risks hiding a voice mishear-disambiguation candidate. See ADR-038.
package headroom

import (
	"strings"
	"unicode"
)

// Config tunes compression. The zero value is a no-op (CompressRows returns the
// input rows unchanged), so callers can gate the feature with a single flag.
type Config struct {
	// MaxContentChars caps each row's Content after markdown-noise stripping.
	// 0 disables the LossyCompress step (content passes through whole).
	MaxContentChars int
}

// Row is the compressible projection of a memory item. Callers build these from
// MemoryItem, run CompressRows, then serialize. Content is replaced in place
// with its lossy-compressed form; Truncated records whether the body was cut
// (the model reads this as "more is available via ID").
type Row struct {
	ID        string
	Kind      string
	Scope     string
	Title     string
	Content   string
	Truncated bool
}

// Result is the output of CompressRows: the (possibly compressed) rows, plus
// the fields that were constant across all of them and so factored out.
type Result struct {
	Rows []Row
	// Constant holds fields whose value was identical across every row. Keys
	// are a subset of {"kind","scope"}. Empty when nothing was uniform. A
	// factored field is blanked on each Row so the caller's serializer omits it
	// (the value lives once in Constant instead).
	Constant map[string]string
}

// CompressRows lossy-compresses each row's Content to the budget, then factors
// uniform fields. With a zero Config it returns the rows unchanged and an empty
// Result.Constant.
func CompressRows(rows []Row, cfg Config) Result {
	if len(rows) == 0 {
		return Result{Rows: rows}
	}
	if cfg.MaxContentChars <= 0 {
		return Result{Rows: rows}
	}

	out := make([]Row, len(rows))
	for i, r := range rows {
		crushed, truncated := LossyCompress(r.Content, cfg.MaxContentChars)
		r.Content = crushed
		r.Truncated = r.Truncated || truncated
		out[i] = r
	}
	return Result{Rows: out, Constant: factor(out)}
}

// factor returns the fields that are identical across every row, blanking them
// on each row so a downstream omitempty serializer drops them. Only the
// low-cardinality structural fields (kind, scope) are eligible — title and
// content are always per-row. Returns nil when nothing is uniform, so callers
// can skip emitting an empty header.
func factor(rows []Row) map[string]string {
	if len(rows) < 2 {
		return nil
	}
	uniform := func(get func(Row) string) (string, bool) {
		v := get(rows[0])
		if v == "" {
			return "", false
		}
		for _, r := range rows[1:] {
			if get(r) != v {
				return "", false
			}
		}
		return v, true
	}
	var constant map[string]string
	if v, ok := uniform(func(r Row) string { return r.Kind }); ok {
		constant = map[string]string{"kind": v}
		for i := range rows {
			rows[i].Kind = ""
		}
	}
	if v, ok := uniform(func(r Row) string { return r.Scope }); ok {
		if constant == nil {
			constant = map[string]string{}
		}
		constant["scope"] = v
		for i := range rows {
			rows[i].Scope = ""
		}
	}
	return constant
}

// ── LossyCompress: markdown-noise-aware lossy compression of one body ────────

// LossyCompress lossily compresses a single content body: it strips
// token-wasteful markdown noise and truncates the result to maxChars on a word
// boundary. It is lossy by design — markdown structure is discarded and the
// tail is cut — so the result is a compact stand-in, not a faithful copy; the
// full original stays retrievable by id (the reversible CCR contract). It
// reports whether anything was dropped (by stripping past maxChars, or by the
// truncation itself). maxChars <= 0 returns body unchanged.
//
// Stripping is conservative — it removes structure that costs tokens but
// carries little meaning the model needs from a search snippet:
//   - image embeds  ![alt](url)            → alt text only
//   - inline links  [text](url)            → text only
//   - fenced code   ```lang\n…\n```         → «code» placeholder
//   - heading/quote/list markers at line start
//   - collapsed runs of whitespace
//
// It deliberately does NOT touch inline emphasis or the words themselves.
func LossyCompress(body string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		return body, false
	}
	stripped := stripMarkdown(body)
	if len(stripped) <= maxChars {
		// Stripping alone may have shortened the original; only call it
		// truncated when the model is actually missing words, which here it is
		// not (the preview conveys the full body's prose).
		return stripped, false
	}
	return truncateAtBoundary(stripped, maxChars) + "…", true
}

func stripMarkdown(s string) string {
	s = stripFences(s)
	var b strings.Builder
	b.Grow(len(s))
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		// Image: ![alt](url) → alt
		if r == '!' && i+1 < len(runes) && runes[i+1] == '[' {
			alt, url, n := parseLink(runes[i+1:])
			if n > 0 {
				b.WriteString(alt)
				_ = url
				i += n // consume "[...](...)"; the leading '!' is skipped
				continue
			}
		}
		// Link: [text](url) → text
		if r == '[' {
			text, url, n := parseLink(runes[i:])
			if n > 0 {
				b.WriteString(text)
				_ = url
				i += n - 1
				continue
			}
		}
		b.WriteRune(r)
	}
	return collapseWhitespace(stripLineMarkers(b.String()))
}

// stripFences replaces ```…``` fenced code blocks with a «code» placeholder.
func stripFences(s string) string {
	const fence = "```"
	var b strings.Builder
	for {
		i := strings.Index(s, fence)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		rest := s[i+len(fence):]
		j := strings.Index(rest, fence)
		if j < 0 {
			// Unterminated fence — keep the rest verbatim (don't eat content).
			b.WriteString(fence)
			b.WriteString(rest)
			break
		}
		b.WriteString("«code»")
		s = rest[j+len(fence):]
	}
	return b.String()
}

// parseLink parses a markdown link starting at runes[0]=='[' and returns its
// display text, URL, and the number of runes consumed (0 if not a well-formed
// link). It does not recurse into nested brackets.
func parseLink(runes []rune) (text, url string, consumed int) {
	if len(runes) == 0 || runes[0] != '[' {
		return "", "", 0
	}
	close := -1
	for i := 1; i < len(runes); i++ {
		if runes[i] == ']' {
			close = i
			break
		}
		if runes[i] == '[' || runes[i] == '\n' {
			return "", "", 0
		}
	}
	if close < 0 || close+1 >= len(runes) || runes[close+1] != '(' {
		return "", "", 0
	}
	paren := -1
	for i := close + 2; i < len(runes); i++ {
		if runes[i] == ')' {
			paren = i
			break
		}
		if runes[i] == '\n' {
			return "", "", 0
		}
	}
	if paren < 0 {
		return "", "", 0
	}
	return string(runes[1:close]), string(runes[close+2 : paren]), paren + 1
}

// stripLineMarkers removes leading heading (#), blockquote (>), and list
// (-,*,+,1.) markers from each line.
func stripLineMarkers(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		t := strings.TrimLeft(line, " \t")
		switch {
		case strings.HasPrefix(t, "#"):
			t = strings.TrimLeft(t, "#")
			t = strings.TrimLeft(t, " ")
		case strings.HasPrefix(t, ">"):
			t = strings.TrimLeft(t, ">")
			t = strings.TrimLeft(t, " ")
		case strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "), strings.HasPrefix(t, "+ "):
			t = t[2:]
		default:
			t = stripOrderedMarker(t)
		}
		lines[i] = t
	}
	return strings.Join(lines, "\n")
}

// stripOrderedMarker removes a leading "N. " ordered-list marker.
func stripOrderedMarker(t string) string {
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(t) && t[i] == '.' && t[i+1] == ' ' {
		return t[i+2:]
	}
	return t
}

// collapseWhitespace replaces runs of whitespace (including newlines) with a
// single space and trims the ends.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			inSpace = true
			continue
		}
		if inSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncateAtBoundary cuts s to at most maxChars runes, backing up to the last
// word boundary so a word is not split. If there is no boundary in range it
// hard-cuts at maxChars.
func truncateAtBoundary(s string, maxChars int) string {
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	cut := maxChars
	for cut > 0 && !unicode.IsSpace(runes[cut]) {
		cut--
	}
	if cut == 0 {
		cut = maxChars // single very long token — hard cut
	}
	return strings.TrimRight(string(runes[:cut]), " ")
}
