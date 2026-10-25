// Package ui delivers the recall graph as an MCP App (SEP-1865): a tool that
// renders an interactive d3-force doc/code graph inline in MCP hosts, plus the
// same page served at GET /ui for browser/debug use.
//
// The app is fully self-contained and OFFLINE — d3 and the @modelcontextprotocol
// /ext-apps SDK are vendored under assets/ and INLINED into the single HTML
// document at serve time, so it works inside a sandboxed iframe with no network
// and no CDN. The same inlined document is what GET /ui returns for browsers.
package ui

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
)

// resourceURI is the ui:// resource the app is served from; show_code_graph links
// to it. The tool→resource link uses the NESTED _meta shape {"ui":{"resourceUri":
// …}} — the form hosts (Claude, VS Code) actually render from. (The ext-apps
// SDK's flat "ui/resourceUri" constant is NOT what the host binds on.)
const (
	resourceURI  = "ui://recall/graph"
	resourceMIME = "text/html;profile=mcp-app"
)

//go:embed assets/index.html
var indexHTML string

//go:embed assets/d3.v7.min.js
var d3JS string

// The About sheet ships INSIDE the binary and is served from this same origin.
// It documents the extraction layer this UI is drawing; hosting it here (rather
// than linking out) keeps the page reachable on a machine with no network and
// keeps the UI's About control pointing at the build it actually belongs to.
//
//go:embed assets/about.html
var aboutHTML string

// page is the HTML with d3 INLINED, built once. Inlining d3 makes the document
// self-contained for the sandboxed MCP iframe (no relative fetch). The MCP bridge
// is raw postMessage (no SDK), which every host supports without extra deps.
var (
	pageOnce sync.Once
	pageHTML string
)

func builtPage() string {
	pageOnce.Do(func() {
		pageHTML = strings.Replace(indexHTML,
			`<script src="./d3.v7.min.js"></script>`,
			"<script>"+d3JS+"</script>", 1)
	})
	return pageHTML
}

// Register wires the MCP App onto an existing MCPServer: the ui:// resource, the
// show_code_graph tool that renders it, and the code_graph_data tool the inline
// app calls over the bridge to load the graph dump.
func Register(s *server.MCPServer, g *graph.Graph) {
	// The UI resource — its body is the self-contained app document.
	res := mcp.NewResource(resourceURI, "X-Ray Doc/Code Graph",
		mcp.WithMIMEType(resourceMIME),
		mcp.WithResourceDescription("Interactive d3-force graph of the documentation and code graph."),
	)
	s.AddResource(res, func(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{URI: resourceURI, MIMEType: resourceMIME, Text: builtPage()},
		}, nil
	})

	// show_code_graph — the MCP App entry point. _meta["ui/resourceUri"] tells the
	// host to render the ui:// resource inline. The result also carries the graph
	// dump as structuredContent so the app can render immediately, plus a text
	// fallback for non-UI hosts.
	showTool := mcp.NewTool("show_code_graph",
		mcp.WithDescription("Open the interactive Doc/Code graph — a visual map of the documentation (ADRs/READMEs) and code symbols and how they connect. Use when the user asks to see, visualize, browse, or explore the graph, docs, or codebase structure."),
	)
	showTool.Meta = mcp.NewMetaFromMap(map[string]any{
		"ui": map[string]any{"resourceUri": resourceURI},
	})
	s.AddTool(showTool, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dump := g.Dump()
		res := mcp.NewToolResultText("Interactive Doc/Code graph is rendered above. " +
			itoa(len(dump.Nodes)) + " nodes, " + itoa(len(dump.Edges)) + " edges.")
		res.StructuredContent = dump
		return res, nil
	})

	// code_graph_data — no UI; the inline app calls this over the bridge to fetch
	// the whole-graph dump (titles + edges, no bodies). Mirrors GET /graph.
	dataTool := mcp.NewTool("code_graph_data",
		mcp.WithDescription("Return the whole code/doc graph as JSON (nodes: qn/kind/title/file/line; edges: source/target/kind). Lean — no node bodies; fetch one node's content with get_graph_node."),
	)
	s.AddTool(dataTool, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dump := g.Dump()
		b, err := json.Marshal(dump)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		res := mcp.NewToolResultText(string(b))
		res.StructuredContent = dump
		return res, nil
	})
}

// Handler serves the app for BROWSER/debug mode: GET /ui returns the inlined,
// self-contained page (works without the MCP bridge — it falls back to fetching
// /graph and /node on this same origin). The vendored libs are also served as
// separate files for completeness, though the inlined page needs no extra fetch.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ui", func(w http.ResponseWriter, _ *http.Request) {
		// Both pages are compiled into the binary, so the only way they change is
		// a rebuild — and then the browser must not keep showing the previous
		// build's page. Without this the heuristic cache wins and a restarted
		// server looks like an edit that never landed.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(builtPage()))
	})
	mux.HandleFunc("/ui/about", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(aboutHTML))
	})
	mux.HandleFunc("/ui/d3.v7.min.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(d3JS))
	})
	return mux
}

// itoa avoids pulling strconv just for two small ints in the result string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
