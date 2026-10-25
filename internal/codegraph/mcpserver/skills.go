package mcpserver

// Claude Code skill retrieval over MCP. Skills are Markdown operating
// procedures for this environment — today distributed by `xray-memory install`
// copying a handful of embedded starter files into a repo's .claude/skills/
// (cmd/recall/install.go). list_skills/get_skill expose the same KIND of
// content a different way: a dedicated "skills" map, built like any other map
// from a real directory (the user's actual ~/.claude/skills tree — one
// directory per skill, SKILL.md inside, following Claude Code's own layout),
// loaded through the existing Resolver like the code/doc maps. No new storage;
// a skill is just a "Doc" node in that map's graph (internal/recall/docs).

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/docs"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
)

// skillPreambleTitle mirrors docs.preambleTitle (unexported): the parser's
// stand-in name for a Doc's leading chunk before its first "## " heading —
// frontmatter, the H1 title, and any intro prose. docs.go deliberately picks a
// title no real author would write, so it can never collide with a genuine
// section; skillMarkdown relies on that same guarantee to know which chunk has
// no heading line of its own to restore.
const skillPreambleTitle = "(preamble)"

// registerSkillTools adds list_skills and get_skill when a skills map is
// configured AND the server can resolve maps by label at all — the same two
// preconditions Resolved/Resolver exist to satisfy. Called from NewReadOnly
// (Mode B); Mode A's single fixed graph (New) has no Resolver to route
// through and never calls this.
func registerSkillTools(s *server.MCPServer, d Deps) {
	if d.Resolver == nil || d.SkillsMap == "" {
		return
	}
	registerListSkills(s, d)
	registerGetSkill(s, d)
}

// resolveSkillsMap resolves the skills map by its EXACT label. Resolve's
// fallback for a name that isn't registered is semantic routing to the
// closest OTHER map — silently answering list_skills/get_skill out of, say,
// the code graph instead. That would read as "these are the skills" when they
// are not, so a label mismatch here is reported as "not registered", never
// swapped for a different map.
func resolveSkillsMap(ctx context.Context, d Deps) (Resolved, error) {
	resolved, err := d.Resolver.Resolve(ctx, d.SkillsMap, "")
	if err != nil {
		return Resolved{}, fmt.Errorf("skills map %q is not registered: %w", d.SkillsMap, err)
	}
	if len(resolved) == 0 || resolved[0].Label != d.SkillsMap {
		return Resolved{}, fmt.Errorf("skills map %q is not registered on this server", d.SkillsMap)
	}
	return resolved[0], nil
}

// isSkillFile reports whether a Doc node's file path is a SKILL.md — the only
// file that names a skill. Claude Code's layout is one directory per skill
// (~/.claude/skills/<name>/SKILL.md), but a skill directory may also hold
// supporting docs alongside it (e.g. <name>/references/*.md); those parse
// into Doc nodes in the same map and must not be counted as skills of their
// own. Case-insensitive: SKILL.md, skill.md, and Skill.MD all count.
func isSkillFile(filePath string) bool {
	return strings.EqualFold(path.Base(filePath), "SKILL.md")
}

// skillDocNodes returns every Doc node in g that is a real skill (a SKILL.md
// file) — the set list_skills/get_skill operate over. This is the fix for a
// live defect: enumerating every docs.KindDoc node unfiltered also picked up
// each skill's supporting reference docs (<name>/references/*.md), which are
// Doc nodes too and, having no containing skill directory of their own,
// named themselves "references" via the directory-fallback — 17 phantom
// "references" entries out of 28 list_skills results on a corpus with seven
// skills that carry reference material. Filtering to SKILL.md alone is what
// makes a "skill" match Claude Code's own definition of one. Reference docs
// are NOT removed from the graph and stay reachable via search_graph_docs —
// this filter is scoped to these two tools only.
func skillDocNodes(g *graph.Graph) []*graph.Node {
	var out []*graph.Node
	for _, n := range g.Nodes() {
		if n.Kind == docs.KindDoc && isSkillFile(n.FilePath) {
			out = append(out, n)
		}
	}
	return out
}

// skillReferenceIndex groups every non-skill Doc node (a skill's supporting
// reference material, e.g. <name>/references/*.md) under its owning skill's
// own directory (<name>), so a skill's list_skills/get_skill entry can name
// its own reference docs by qn — keyed by path.Dir(skill.FilePath), matching
// how callers already look this index up. Doc nodes are not linked to one
// another across files in this graph — the "contains" edge only reaches a
// Doc's own Section children — so directory co-location is what associates a
// reference doc with its skill. That co-location is a subtree relationship,
// not an exact-directory match: a reference doc one level deeper, under
// <name>/references/, shares no directory string with <name>/SKILL.md, only
// a path PREFIX, so this walks the known skill directories and matches by
// prefix rather than grouping by each doc's own immediate directory (which
// would key <name>/references/*.md under "<name>/references" and leave
// <name>'s entry always empty). A directory with no matching non-SKILL.md
// Doc node is simply absent from the map, so callers check with a plain
// lookup.
func skillReferenceIndex(g *graph.Graph) map[string][]string {
	var skillDirs []string
	for _, n := range skillDocNodes(g) {
		if dir := path.Dir(n.FilePath); dir != "" && dir != "." {
			skillDirs = append(skillDirs, dir)
		}
	}
	idx := map[string][]string{}
	for _, n := range g.Nodes() {
		if n.Kind != docs.KindDoc || isSkillFile(n.FilePath) {
			continue
		}
		for _, dir := range skillDirs {
			if strings.HasPrefix(n.FilePath, dir+"/") {
				idx[dir] = append(idx[dir], n.QN)
				break // first (and structurally only) enclosing skill dir owns this doc
			}
		}
	}
	for dir := range idx {
		sort.Strings(idx[dir])
	}
	return idx
}

// findSkillNode resolves name against g's real skills (skillDocNodes) only —
// frontmatter name first, directory-fallback name second, same precedence
// get_skill has always used. Extracted out of registerGetSkill so the
// resolution rule (in particular, that a reference doc's containing
// directory, e.g. "references", can never itself resolve — the fix for the
// arbitrary-match defect this function replaces) is directly testable
// without standing up the MCP server or a Resolver.
func findSkillNode(g *graph.Graph, name string) (n *graph.Node, fm skillFrontmatter, content, displayName string, ok bool) {
	want := normalizeSkillName(name)
	for _, cand := range skillDocNodes(g) {
		candContent := skillMarkdown(g, cand)
		candFM := parseSkillFrontmatter(candContent)
		dirName := skillDirFallback(cand.FilePath)
		// Frontmatter name first, directory name second — a caller may
		// reasonably pass either, and a file's frontmatter name can drift
		// from its directory.
		matched := (candFM.Name != "" && normalizeSkillName(candFM.Name) == want) || normalizeSkillName(dirName) == want
		if !matched {
			continue
		}
		display := candFM.Name
		if display == "" {
			display = dirName
		}
		return cand, candFM, candContent, display, true
	}
	return nil, skillFrontmatter{}, "", "", false
}

// list_skills — every operating procedure available in this environment.
func registerListSkills(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("list_skills",
		readOnlyTool(),
		mcp.WithDescription("List every operating procedure available in this environment — Claude Code skills: packaged how-tos for a recurring kind of task (deploying, auditing a repo, writing a memory, building a map, and so on). Returns each skill's name, description, and qn, plus a \"references\" list of qns for any supporting docs the skill carries alongside it (fetch one via get_graph_node). Call this to see what procedures exist and which one fits before doing something ad hoc; feed a name to get_skill to read its actual steps."),
	), logged(d, "list_skills", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		r, err := resolveSkillsMap(ctx, d)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		refIndex := skillReferenceIndex(r.Graph)
		var skills []map[string]any
		for _, n := range skillDocNodes(r.Graph) {
			fm := parseSkillFrontmatter(skillMarkdown(r.Graph, n))
			name := fm.Name
			if name == "" {
				name = skillDirFallback(n.FilePath)
			}
			entry := map[string]any{
				"name":        name,
				"description": fm.Description,
				"qn":          n.QN,
			}
			if refs := refIndex[path.Dir(n.FilePath)]; len(refs) > 0 {
				entry["references"] = refs
			}
			skills = append(skills, entry)
		}
		sort.Slice(skills, func(i, j int) bool { return skills[i]["name"].(string) < skills[j]["name"].(string) })
		return jsonResult(map[string]any{"count": len(skills), "skills": skills})
	}))
}

// get_skill — one skill's full markdown, by name.
func registerGetSkill(s *server.MCPServer, d Deps) {
	s.AddTool(mcp.NewTool("get_skill",
		readOnlyTool(),
		mcp.WithDescription("Return one operating procedure's full markdown, frontmatter included, by the skill name from list_skills (or a name you already know). Includes a \"references\" list of qns for any supporting docs the skill carries alongside it, if any (fetch one via get_graph_node). Use this once you know which skill applies and need its actual steps, not just that it exists."),
		mcp.WithString("name", mcp.Description("Skill name, e.g. list_skills' \"name\" field. Case-insensitive; a .md suffix is ignored."), mcp.Required()),
	), logged(d, "get_skill", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, _ := req.GetArguments()["name"].(string)
		if name == "" {
			return mcp.NewToolResultError("name is required"), nil
		}
		r, err := resolveSkillsMap(ctx, d)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		n, fm, content, displayName, ok := findSkillNode(r.Graph, name)
		if !ok {
			return mcp.NewToolResultError("skill not found: " + name), nil
		}
		result := map[string]any{
			"name":        displayName,
			"description": fm.Description,
			"qn":          n.QN,
			"content":     content,
		}
		if refs := skillReferenceIndex(r.Graph)[path.Dir(n.FilePath)]; len(refs) > 0 {
			result["references"] = refs
		}
		return jsonResult(result)
	}))
}

// skillMarkdown reconstructs a skill Doc's full markdown from its Section
// children (in source order, via "contains" edges). The shared nodeContent
// helper (used by get_graph_node) concatenates section BODIES only — the docs
// parser strips each "## " heading line out of its own Section's Body, which
// is fine for a search preview but would silently drop every heading from an
// operating procedure. This restores them, so the result matches the source
// file in everything but exact original whitespace.
func skillMarkdown(g *graph.Graph, doc *graph.Node) string {
	var b strings.Builder
	for _, e := range g.Callees(doc.QN, "contains") {
		c, ok := g.Get(e.Target)
		if !ok || c.Kind != docs.KindSection {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if c.Name != skillPreambleTitle {
			b.WriteString("## ")
			b.WriteString(c.Name)
			b.WriteString("\n\n")
		}
		b.WriteString(c.Body)
	}
	return strings.TrimSpace(b.String())
}

// skillFrontmatter is the subset of a SKILL.md's YAML frontmatter that
// list_skills/get_skill care about.
type skillFrontmatter struct {
	Name        string
	Description string
}

// parseSkillFrontmatter reads name/description out of a leading YAML
// frontmatter block ("---\nkey: value\n---"). It is a minimal, stdlib-only
// reader, not a YAML parser: it understands a plain "key: value" line (quoted
// or not, and tolerant of a colon inside the value — only the FIRST colon on
// the line is treated as the key separator) and a folded block scalar
// ("key: >"), whose indented continuation lines are joined with spaces the
// way YAML folds one. The folded form is the common case in real skill
// files — most write "description: >" followed by several indented lines
// rather than one long inline line. Any other key (metadata, allowed-tools,
// version, …), and anything nested under one, is skipped over. Returns the
// zero value when the content has no frontmatter block.
func parseSkillFrontmatter(content string) skillFrontmatter {
	var fm skillFrontmatter
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fm
	}
	for i := 1; i < len(lines); {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			break // end of frontmatter
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			i++ // blank, or indented under a key we're not tracking — skip
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			i++
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		i++
		if isFoldedScalar(val) {
			var parts []string
			for i < len(lines) {
				cl := lines[i]
				if strings.TrimSpace(cl) == "" {
					i++
					continue
				}
				if cl[0] != ' ' && cl[0] != '\t' {
					break // dedented: the folded block ended
				}
				parts = append(parts, strings.TrimSpace(cl))
				i++
			}
			val = strings.Join(parts, " ")
		} else {
			val = unquoteYAML(val)
		}
		switch key {
		case "name":
			fm.Name = val
		case "description":
			fm.Description = val
		}
	}
	return fm
}

// isFoldedScalar reports whether a frontmatter value is a YAML block-scalar
// indicator (its real text lives on the indented lines that follow) rather
// than an inline value.
func isFoldedScalar(v string) bool {
	switch v {
	case ">", ">-", ">+", "|", "|-", "|+":
		return true
	}
	return false
}

// unquoteYAML strips one matching pair of surrounding quotes, if present.
func unquoteYAML(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// skillDirFallback names a skill by its containing directory when the
// frontmatter has no "name" field — Claude Code's own skill layout is one
// directory per skill, ~/.claude/skills/<dir>/SKILL.md.
func skillDirFallback(filePath string) string {
	dir := path.Dir(filePath)
	if dir != "" && dir != "." {
		return path.Base(dir)
	}
	base := path.Base(filePath)
	return strings.TrimSuffix(base, path.Ext(base))
}

// normalizeSkillName canonicalizes a skill name for matching: case-insensitive
// and tolerant of a caller passing the file's own name (code-memory.md)
// instead of its frontmatter/directory name (code-memory).
func normalizeSkillName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".md")
}
