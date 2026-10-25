// Package docs ingests Markdown documentation (ADRs, todos, READMEs) into the
// same node/edge model as code. Each file becomes one Doc node; each
// "## " heading becomes a Section node linked by a contains edge. Docs are not
// Tree-sitter parsed — Markdown structure is heading-based, so a simple chunker
// suffices. The resulting nodes flow through the same graph + embedding index as
// code, so semantic search spans code and docs together.
package docs

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Node kinds emitted for documentation.
const (
	KindDoc     = "Doc"
	KindSection = "Section"
)

// Parse turns one Markdown file's bytes into a Doc node plus Section nodes.
// path is the repo-relative file path; it keys the Doc node.
func Parse(path string, source []byte) parser.Result {
	res := parser.Result{}

	title := firstTitle(source, path)
	totalLines := bytes.Count(source, []byte("\n")) + 1

	secs := sections(source)

	// The Doc node's embeddable content is the title plus the list of section
	// headings — a compact summary that helps whole-doc retrieval.
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.title)
	}
	doc := parser.NodeInfo{
		Kind:      KindDoc,
		Name:      title,
		FilePath:  path,
		Language:  "markdown",
		LineStart: 1,
		LineEnd:   totalLines,
		Content:   title + " — sections: " + strings.Join(headings, ", "),
	}
	res.Nodes = append(res.Nodes, doc)

	for _, sec := range secs {
		node := parser.NodeInfo{
			Kind:       KindSection,
			Name:       sec.title,
			FilePath:   path,
			Language:   "markdown",
			LineStart:  sec.startLine,
			LineEnd:    sec.endLine,
			ParentName: title, // section qualifies under the doc title
			// Content = lean, capped embedding/preview text (with heading context).
			Content: title + " › " + sec.title + "\n" + capChars(sec.body, maxSectionBodyChars),
			// Body = full, clean section text returned by get_code_node.
			Body: sec.body,
		}
		res.Nodes = append(res.Nodes, node)
		res.Edges = append(res.Edges, parser.EdgeInfo{
			Kind:   parser.EdgeContains,
			Source: path,
			Target: node.Qualified(),
			File:   path,
			Line:   sec.startLine,
		})
	}
	return res
}

// isFenceDelimiter reports whether a line opens or closes a fenced code block
// (``` or ~~~, optionally indented and followed by an info string).
func isFenceDelimiter(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// firstTitle returns the first H1 ("# ..."), else the file path's base.
//
// Lines inside fenced code blocks are skipped. A shell comment such as
// "# build backend deps ..." inside a ``` block is not a heading, and treating
// it as one silently mis-titles the document: the Doc node's embeddable content
// is built from the title, so the node ends up describing a code comment rather
// than the document. Observed on a real memory file whose title became
// "build backend deps must be installed manually when using --no-build-isolation:"
// — see docs/todo/cc-memory-eval.md.
func firstTitle(source []byte, path string) string {
	sc := bufio.NewScanner(bytes.NewReader(source))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inFence := false
	for sc.Scan() {
		line := sc.Text()
		if isFenceDelimiter(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return path
}

type section struct {
	title     string
	startLine int
	endLine   int
	body      string
}

// maxSectionBodyChars caps section prose fed to the embedder; the nomic context
// window is finite and the heading + opening paragraphs carry most of the signal.
const maxSectionBodyChars = 2000

// preambleTitle labels the content before the first "## " heading (title, status
// block, intro). It is intentionally NOT a normal heading name so it can never
// collide with a real "## Overview" / "## Intro" section in the document.
const preambleTitle = "(preamble)"

// sections splits a Markdown document on "## " (H2) headings. The preamble
// (content before the first H2) becomes a "(preamble)" section, but only if it
// carries substantive prose — a bare title + status/--- front-matter is dropped,
// so a doc whose first real section is "## Overview" yields one Overview, not two.
// H3+ stay within their parent H2 section.
//
// Headings inside fenced code blocks are ignored, so a "## " line in a sample
// document, a diff, or a shell comment cannot split the document mid-block and
// invent a section whose title is a line of code.
func sections(source []byte) []section {
	lines := strings.Split(string(source), "\n")
	var secs []section
	cur := section{title: preambleTitle, startLine: 1}
	var body strings.Builder
	inFence := false

	flush := func(endLine int) {
		cur.endLine = endLine
		// Store the FULL body (uncapped) — it is the display text. The embedding
		// text is capped separately when the node's Content is built.
		cur.body = strings.TrimSpace(body.String())
		// Keep real "## " sections always; keep the preamble only if it has prose
		// beyond title/metadata front-matter.
		if cur.title != preambleTitle || hasProse(cur.body) {
			secs = append(secs, cur)
		}
		body.Reset()
	}

	for i, line := range lines {
		lineNo := i + 1
		if isFenceDelimiter(line) {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "### ") {
			flush(lineNo - 1)
			cur = section{title: strings.TrimSpace(line[3:]), startLine: lineNo}
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush(len(lines))
	return secs
}

// hasProse reports whether text contains a real prose line — i.e. a non-empty
// line that is not the H1 title, a "**Field:** value" metadata line, or a "---"
// rule. Used to drop a content-free preamble (just front-matter).
func hasProse(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t == "---" {
			continue
		}
		if strings.HasPrefix(t, "# ") { // H1 title
			continue
		}
		if strings.HasPrefix(t, "**") && strings.Contains(t, ":") { // **Status:** … metadata
			continue
		}
		return true
	}
	return false
}

func capChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
