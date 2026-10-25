package mcpserver

import (
	"sort"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/docs"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// buildSkillsFixtureGraph builds a small graph mirroring the real defect: two
// genuine skills (one directory each, one SKILL.md inside) plus one of them
// carrying a supporting reference doc one level deeper, at
// <skill>/references/*.md — the exact shape that made 17 of 28 live
// list_skills entries come back named "references" before this fix (see the
// skillDocNodes doc comment). Built through docs.Parse + graph.Build, the
// same path real files take, rather than hand-built Node structs, so the
// fixture exercises the real Doc/Section shape (frontmatter, "contains"
// edges) skillMarkdown/parseSkillFrontmatter depend on.
func buildSkillsFixtureGraph(t *testing.T) *graph.Graph {
	t.Helper()
	files := map[string]string{
		"google-agents-cli-deploy/SKILL.md": "---\n" +
			"name: google-agents-cli-deploy\n" +
			"description: >\n" +
			"  Deploy an ADK agent to Cloud Run or GKE.\n" +
			"---\n\n" +
			"## Steps\n\n" +
			"Run the deploy command.\n",
		"google-agents-cli-deploy/references/adk-code.md": "# ADK reference\n\n" +
			"Supporting detail for the deploy skill.\n",
		"xray-memory/SKILL.md": "# xray as the recall layer\n\n" +
			"Search memory before answering.\n",
	}
	var nodes []parser.NodeInfo
	var edges []parser.EdgeInfo
	for path, content := range files {
		res := docs.Parse(path, []byte(content))
		nodes = append(nodes, res.Nodes...)
		edges = append(edges, res.Edges...)
	}
	g := graph.New()
	g.Build(graph.RepoGraph{Nodes: nodes, Edges: edges})
	return g
}

// TestSkillDocNodes_FiltersOutReferenceDocs is the regression test for the
// live defect: skillDocNodes (and therefore list_skills) must list only the
// two real SKILL.md files, never the references/*.md Doc node alongside one
// of them.
func TestSkillDocNodes_FiltersOutReferenceDocs(t *testing.T) {
	g := buildSkillsFixtureGraph(t)
	var gotPaths []string
	for _, n := range skillDocNodes(g) {
		gotPaths = append(gotPaths, n.FilePath)
	}
	sort.Strings(gotPaths)
	want := []string{"google-agents-cli-deploy/SKILL.md", "xray-memory/SKILL.md"}
	if len(gotPaths) != len(want) {
		t.Fatalf("skillDocNodes() = %v (len %d), want %v (len %d)", gotPaths, len(gotPaths), want, len(want))
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("skillDocNodes()[%d] = %q, want %q", i, gotPaths[i], want[i])
		}
	}
}

// TestFindSkillNode_ReferencesDoesNotResolve is the regression test for the
// get_skill half of the defect: before the fix, get_skill("references") would
// arbitrarily resolve to whichever references/*.md Doc node the unfiltered
// scan hit first (its directory-fallback name is "references", same as every
// other reference doc). After the fix, "references" names no skill at all —
// findSkillNode only ever matches a skillDocNodes candidate.
func TestFindSkillNode_ReferencesDoesNotResolve(t *testing.T) {
	g := buildSkillsFixtureGraph(t)
	if _, _, _, _, ok := findSkillNode(g, "references"); ok {
		t.Error(`findSkillNode(g, "references") resolved, want not-found`)
	}
}

// TestFindSkillNode_RealSkillsStillResolve guards against the filter being
// too aggressive: both real skills must still resolve by name, one via its
// frontmatter name and one via the directory-fallback (it has none).
func TestFindSkillNode_RealSkillsStillResolve(t *testing.T) {
	g := buildSkillsFixtureGraph(t)

	n, fm, content, displayName, ok := findSkillNode(g, "google-agents-cli-deploy")
	if !ok {
		t.Fatal(`findSkillNode(g, "google-agents-cli-deploy") did not resolve`)
	}
	if displayName != "google-agents-cli-deploy" {
		t.Errorf("displayName = %q, want %q", displayName, "google-agents-cli-deploy")
	}
	if fm.Description != "Deploy an ADK agent to Cloud Run or GKE." {
		t.Errorf("Description = %q, want the frontmatter description", fm.Description)
	}
	if content == "" {
		t.Error("content is empty, want the reconstructed markdown")
	}
	if n.FilePath != "google-agents-cli-deploy/SKILL.md" {
		t.Errorf("FilePath = %q, want %q", n.FilePath, "google-agents-cli-deploy/SKILL.md")
	}

	if _, _, _, displayName, ok := findSkillNode(g, "xray-memory"); !ok || displayName != "xray-memory" {
		t.Errorf(`findSkillNode(g, "xray-memory") = (ok=%v, displayName=%q), want (true, "xray-memory")`, ok, displayName)
	}
}

// TestSkillReferenceIndex_AssociatesNestedReferenceDocs guards the
// reference-discoverability addition: a reference doc one level deeper than
// its skill (<skill>/references/*.md) must be indexed under the skill's OWN
// directory (<skill>), not under its own immediate directory
// (<skill>/references) — the two differ, so a naive exact-directory grouping
// would leave the skill's entry always empty. The skill with no reference
// docs (xray-memory) must have no entry at all.
func TestSkillReferenceIndex_AssociatesNestedReferenceDocs(t *testing.T) {
	g := buildSkillsFixtureGraph(t)
	idx := skillReferenceIndex(g)

	refs := idx["google-agents-cli-deploy"]
	if len(refs) != 1 {
		t.Fatalf(`skillReferenceIndex(g)["google-agents-cli-deploy"] = %v, want exactly 1 entry`, refs)
	}
	if refs[0] != "google-agents-cli-deploy/references/adk-code.md" {
		t.Errorf("reference qn = %q, want %q", refs[0], "google-agents-cli-deploy/references/adk-code.md")
	}

	if refs := idx["xray-memory"]; len(refs) != 0 {
		t.Errorf(`skillReferenceIndex(g)["xray-memory"] = %v, want none`, refs)
	}
}

func TestParseSkillFrontmatter_Normal(t *testing.T) {
	content := "---\nname: xray-memory\ndescription: >\n  Use xray as the primary recall layer.\n  Search it before answering.\n---\n\n# xray as the recall layer\n"
	fm := parseSkillFrontmatter(content)
	if fm.Name != "xray-memory" {
		t.Errorf("Name = %q, want %q", fm.Name, "xray-memory")
	}
	want := "Use xray as the primary recall layer. Search it before answering."
	if fm.Description != want {
		t.Errorf("Description = %q, want %q", fm.Description, want)
	}
}

func TestParseSkillFrontmatter_NoFrontmatter(t *testing.T) {
	content := "# Just a doc\n\nNo frontmatter block here at all.\n"
	fm := parseSkillFrontmatter(content)
	if fm.Name != "" || fm.Description != "" {
		t.Errorf("got %+v, want zero value", fm)
	}
}

func TestParseSkillFrontmatter_QuotedDescription(t *testing.T) {
	content := "---\nname: audit\ndescription: \"Run a security audit\"\n---\n"
	fm := parseSkillFrontmatter(content)
	if fm.Description != "Run a security audit" {
		t.Errorf("Description = %q, want %q (quotes should be stripped)", fm.Description, "Run a security audit")
	}
}

func TestParseSkillFrontmatter_DescriptionContainsColon(t *testing.T) {
	content := "---\nname: deploy\ndescription: Use when: the build passes, then ship it.\n---\n"
	fm := parseSkillFrontmatter(content)
	want := "Use when: the build passes, then ship it."
	if fm.Description != want {
		t.Errorf("Description = %q, want %q (only the first colon should split key from value)", fm.Description, want)
	}
}

// Real skill files in ~/.claude/skills overwhelmingly write their description
// as a YAML folded block scalar ("description: >" plus indented continuation
// lines), not one inline line — this is the dominant case in the corpus this
// parser was built to read, so it gets its own test beyond the basic "Normal"
// case above.
func TestParseSkillFrontmatter_FoldedDescriptionWithColon(t *testing.T) {
	content := "---\nname: google-agents-cli-deploy\ndescription: >\n  This skill should be used when the user wants to \"deploy an agent\",\n  or needs guidance on: Agent Runtime, Cloud Run, or GKE deployment targets.\n  Covers deployment workflows and rollback.\n---\n"
	fm := parseSkillFrontmatter(content)
	want := `This skill should be used when the user wants to "deploy an agent", or needs guidance on: Agent Runtime, Cloud Run, or GKE deployment targets. Covers deployment workflows and rollback.`
	if fm.Description != want {
		t.Errorf("Description = %q, want %q", fm.Description, want)
	}
}

func TestParseSkillFrontmatter_StopsAtNestedKeys(t *testing.T) {
	// A key we don't track (metadata) with its own nested block must not leak
	// into, or be mistaken for, the fields we do track.
	content := "---\nname: adk-code\ndescription: >\n  Write agent code with ADK.\nmetadata:\n  author: Google\n  version: 0.1.3\n---\n"
	fm := parseSkillFrontmatter(content)
	if fm.Name != "adk-code" {
		t.Errorf("Name = %q, want %q", fm.Name, "adk-code")
	}
	if fm.Description != "Write agent code with ADK." {
		t.Errorf("Description = %q, want %q", fm.Description, "Write agent code with ADK.")
	}
}

func TestSkillDirFallback(t *testing.T) {
	cases := []struct {
		filePath string
		want     string
	}{
		{"xray-memory/SKILL.md", "xray-memory"},
		{"google-agents-cli-deploy/SKILL.md", "google-agents-cli-deploy"},
		{"SKILL.md", "SKILL"}, // no containing directory: falls back to the base name
	}
	for _, c := range cases {
		if got := skillDirFallback(c.filePath); got != c.want {
			t.Errorf("skillDirFallback(%q) = %q, want %q", c.filePath, got, c.want)
		}
	}
}

func TestNormalizeSkillName(t *testing.T) {
	cases := map[string]string{
		"xray-memory":     "xray-memory",
		"Xray-Memory":     "xray-memory",
		"xray-memory.md":  "xray-memory",
		"XRAY-MEMORY.MD":  "xray-memory",
		"  xray-memory  ": "xray-memory",
	}
	for in, want := range cases {
		if got := normalizeSkillName(in); got != want {
			t.Errorf("normalizeSkillName(%q) = %q, want %q", in, got, want)
		}
	}
}
