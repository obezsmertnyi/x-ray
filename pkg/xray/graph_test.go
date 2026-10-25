package xray_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

// graphServer stands in for a graph server. Each reply has the shape the real
// tools produce, and the handlers echo the arguments the SDK sent, so the test
// can check both directions.
func graphServer(t *testing.T) (string, *map[string]map[string]any) {
	t.Helper()
	seen := map[string]map[string]any{}
	s := server.NewMCPServer("graph-stub", "1")
	add := func(name, reply string, opts ...mcp.ToolOption) {
		s.AddTool(mcp.NewTool(name, opts...), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen[name] = req.GetArguments()
			return mcp.NewToolResultText(reply), nil
		})
	}
	hits := `{"query":"q","count":1,"results":[{"qn":"app//a.go::Run","kind":"Func","title":"Run","file":"a.go","line":12,"score":0.91,"preview":"func Run()","repo":"app"}]}`
	add("search_graph", hits)
	add("search_graph_docs", hits)
	add("get_graph_node", `{"qn":"app//a.go::Run","repo":"app","kind":"Func","title":"Run","file":"a.go","line_start":12,"line_end":20,"language":"go","content":"func Run() {}","calls":["app//b.go::Do"],"called_by":["app//main.go::main"]}`)
	add("get_graph_impact", `{"seeds":["app//a.go::Run"],"depth":2,"count":1,"impacted":[{"QN":"app//main.go::main","Depth":1}]}`)
	add("get_graph_risk", `{"QN":"app//a.go::Run","Level":"MEDIUM","DirectCallers":3,"ModuleSpread":2,"ImpactedCount":7,"Modules":["cmd","internal"]}`)
	add("get_graph_stats", `{"stats":{"Nodes":10}}`)
	add("update_code_graph", `{"changed":2,"removed":0,"unowned":["../x.go"]}`)
	add("remember", `{"map":"session","qn":"session//memory/n.md::1","at":"2026-10-25T10:00:00Z","notes":1}`)
	add("recall", `{"map":"session","count":1,"notes":[{"qn":"session//memory/n.md::1","at":"2026-10-25T10:00:00Z","text":"asked about retries","title":"retries","attrs":{"about":"checkout"}}],"note":"notes are what people said"}`)
	add("forget", `{"map":"session","qn":"session//memory/n.md::1","notes":0}`)
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp", &seen
}

func TestGraph_SearchSendsTheOptionsAndDecodesHits(t *testing.T) {
	url, seen := graphServer(t)
	c, err := xray.Connect(context.Background(), url)
	require.NoError(t, err)
	defer c.Close()

	res, err := c.SearchGraph(context.Background(), "who calls Run", xray.OnMap("app"), xray.OfKind("Func"), xray.Limit(3), xray.Cutoff())
	require.NoError(t, err)
	require.Len(t, res.Results, 1)
	h := res.Results[0]
	assert.Equal(t, "app//a.go::Run", h.QN)
	assert.Equal(t, 12, h.Line)
	assert.InDelta(t, 0.91, h.Score, 1e-9)

	sent := (*seen)["search_graph"]
	assert.Equal(t, "who calls Run", sent["query"])
	assert.Equal(t, "app", sent["map"])
	assert.Equal(t, "app", sent["repo"], "a live server reads repo, a snapshot server map")
	assert.Equal(t, "Func", sent["kind"])
	assert.EqualValues(t, 3, sent["limit"])
	assert.Equal(t, true, sent["cutoff"])

	docs, err := c.SearchGraphDocs(context.Background(), "why no repo-wide name matching")
	require.NoError(t, err)
	assert.Equal(t, 1, docs.Count)
}

func TestGraph_NodeImpactRisk(t *testing.T) {
	url, seen := graphServer(t)
	c, err := xray.Connect(context.Background(), url)
	require.NoError(t, err)
	defer c.Close()
	ctx := context.Background()

	n, err := c.Node(ctx, "app//a.go::Run")
	require.NoError(t, err)
	assert.Equal(t, 12, n.LineStart)
	assert.Equal(t, []string{"app//b.go::Do"}, n.Calls)
	assert.Equal(t, []string{"app//main.go::main"}, n.CalledBy)

	imp, err := c.Impact(ctx, []string{"app//a.go::Run"}, 3, 50)
	require.NoError(t, err)
	assert.Equal(t, []xray.ImpactNode{{QN: "app//main.go::main", Depth: 1}}, imp.Impacted)
	assert.EqualValues(t, 3, (*seen)["get_graph_impact"]["depth"])
	assert.EqualValues(t, 50, (*seen)["get_graph_impact"]["max_nodes"])

	r, err := c.Risk(ctx, "app//a.go::Run", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, "MEDIUM", r.Level)
	assert.Equal(t, 3, r.DirectCallers)
	assert.NotContains(t, (*seen)["get_graph_risk"], "depth", "zero means the server default")

	raw, err := c.GraphStats(ctx, xray.OnMap("app"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"stats":{"Nodes":10}}`, string(raw))

	up, err := c.UpdateGraph(ctx, "a.go", "../x.go")
	require.NoError(t, err)
	assert.Equal(t, 2, up.Changed)
	assert.Equal(t, []string{"../x.go"}, up.Unowned)
}

func TestGraph_Notes(t *testing.T) {
	url, seen := graphServer(t)
	c, err := xray.Connect(context.Background(), url)
	require.NoError(t, err)
	defer c.Close()
	ctx := context.Background()

	r, err := c.Remember(ctx, "asked about retries", xray.NoteTitle("retries"), xray.NoteTTL("30d"), xray.NoteAttrs(map[string]string{"about": "checkout"}))
	require.NoError(t, err)
	assert.Equal(t, "session", r.Map)
	sent := (*seen)["remember"]
	assert.Equal(t, "retries", sent["title"])
	assert.Equal(t, "30d", sent["ttl"])
	assert.Equal(t, "about=checkout", sent["attrs"])

	rec, err := c.Recall(ctx, "retries", 5)
	require.NoError(t, err)
	require.Len(t, rec.Notes, 1)
	assert.Equal(t, "checkout", rec.Notes[0].Attrs["about"])

	f, err := c.Forget(ctx, r.QN)
	require.NoError(t, err)
	assert.Equal(t, 0, f.Notes)
}
