package xrayadk_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	xraymem "github.com/triageagent-dev/x-ray/v2/pkg/memory"
	"github.com/triageagent-dev/x-ray/v2/pkg/xray"

	xrayadk "github.com/triageagent-dev/x-ray/adapters/adk"
)

// stub is a small MCP server that behaves like X-Ray's memory tools for what the
// adapter uses: writes are kept by id, searches match by word, and a private
// item is returned only to a caller that passes its subject.
type stub struct {
	items map[string]xray.Item
	order []string
}

func newStub(t *testing.T) (*stub, *xray.Client) {
	t.Helper()
	st := &stub{items: map[string]xray.Item{}}
	s := server.NewMCPServer("stub", "1")
	str := func(a map[string]any, k string) string { v, _ := a[k].(string); return v }
	s.AddTool(mcp.NewTool("upsert_memory"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := req.GetArguments()
		id := str(a, "id")
		if id == "" {
			id = "gen-" + str(a, "title")
		}
		status := "created"
		if _, ok := st.items[id]; ok {
			status = "updated"
		} else {
			st.order = append(st.order, id)
		}
		var tags []string
		if raw, ok := a["tags"].([]any); ok {
			for _, v := range raw {
				tags = append(tags, v.(string))
			}
		}
		st.items[id] = xray.Item{ID: id, Kind: str(a, "kind"), Scope: str(a, "scope"), Subject: str(a, "subject"),
			Title: str(a, "title"), Content: str(a, "content"), Tags: tags, Origin: xray.OriginDerived}
		b, _ := json.Marshal(map[string]any{"id": id, "status": status, "kind": str(a, "kind"), "title": str(a, "title")})
		return mcp.NewToolResultText(string(b)), nil
	})
	s.AddTool(mcp.NewTool("search_memory"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := req.GetArguments()
		subject := str(a, "subject")
		var out []xray.Item
		for _, id := range st.order {
			it, ok := st.items[id]
			if !ok || (it.Scope == xray.ScopePrivate && it.Subject != subject) {
				continue
			}
			for _, w := range strings.Fields(strings.ToLower(str(a, "query"))) {
				if strings.Contains(strings.ToLower(it.Title+" "+it.Content), w) {
					out = append(out, it)
					break
				}
			}
		}
		b, _ := json.Marshal(map[string]any{"count": len(out), "items": out})
		return mcp.NewToolResultText(string(b)), nil
	})
	s.AddTool(mcp.NewTool("refute_memory"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := str(req.GetArguments(), "id")
		delete(st.items, id)
		b, _ := json.Marshal(map[string]any{"id": id, "status": "stale", "cascaded": []string{}})
		return mcp.NewToolResultText(string(b)), nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	t.Cleanup(ts.Close)
	c, err := xray.Connect(context.Background(), ts.URL+"/mcp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return st, c
}

func (st *stub) put(id string, it xray.Item) {
	it.ID = id
	st.items[id] = it
	st.order = append(st.order, id)
}

func sessionWith(t *testing.T, app, user string, turns ...[2]string) session.Session {
	t.Helper()
	ctx := context.Background()
	svc := session.InMemoryService()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: app, UserID: user})
	require.NoError(t, err)
	for _, turn := range turns {
		ev := session.NewEventWithContext(ctx, "inv")
		ev.Author = turn[0]
		ev.LLMResponse = model.LLMResponse{Content: genai.NewContentFromText(turn[1], genai.Role(turn[0]))}
		require.NoError(t, svc.AppendEvent(ctx, resp.Session, ev))
	}
	return resp.Session
}

func TestAddSession_WritesOnePrivateNotePerExchange(t *testing.T) {
	f, c := newStub(t)
	svc := xrayadk.New(c)
	sess := sessionWith(t, "shop", "u1",
		[2]string{"user", "I prefer window seats on night flights."},
		[2]string{"model", "Noted, window seats on night flights."},
		[2]string{"user", "My dog is called Biscuit."},
		[2]string{"model", "Biscuit is a lovely name."},
	)
	require.NoError(t, svc.AddSessionToMemory(context.Background(), sess))
	require.Len(t, f.items, 2)
	for _, it := range f.items {
		assert.Equal(t, xray.ScopePrivate, it.Scope)
		assert.Equal(t, "shop/u1", it.Subject)
		assert.Contains(t, it.Content, "User: ")
		assert.Contains(t, it.Content, "Assistant: ")
	}

	// Adding the same session again updates in place.
	require.NoError(t, svc.AddSessionToMemory(context.Background(), sess))
	assert.Len(t, f.items, 2)
}

func TestSearchMemory_OwnNotesThenSharedAndNothingOfAnotherUser(t *testing.T) {
	ctx := context.Background()
	f, c := newStub(t)
	svc := xrayadk.New(c)
	require.NoError(t, svc.AddSessionToMemory(ctx, sessionWith(t, "shop", "u1",
		[2]string{"user", "My dog is called Biscuit."}, [2]string{"model", "Lovely."})))
	require.NoError(t, svc.AddSessionToMemory(ctx, sessionWith(t, "shop", "u2",
		[2]string{"user", "My dog is called Rex."}, [2]string{"model", "Lovely."})))
	f.put("team-1", xray.Item{Kind: xray.KindStableFact, Scope: xray.ScopePublic,
		Title: "Dog policy", Content: "Dogs are welcome in the office on Fridays.", Origin: xray.OriginDerived})

	res, err := svc.SearchMemory(ctx, &memory.SearchRequest{Query: "dog", AppName: "shop", UserID: "u1"})
	require.NoError(t, err)
	require.Len(t, res.Memories, 2)
	assert.Contains(t, res.Memories[0].Content.Parts[0].Text, "Biscuit")
	assert.Equal(t, "team-1", res.Memories[1].ID)
	assert.Equal(t, xray.OriginDerived, res.Memories[1].CustomMetadata["origin"])
	for _, m := range res.Memories {
		assert.NotContains(t, m.Content.Parts[0].Text, "Rex", "another user's note is not reachable")
	}

	alone, err := xrayadk.New(c, xrayadk.WithoutShared()).SearchMemory(ctx, &memory.SearchRequest{Query: "dog", AppName: "shop", UserID: "u1"})
	require.NoError(t, err)
	assert.Len(t, alone.Memories, 1)
}

// Against a real X-Ray: XRAY_URL=http://localhost:8083/mcp XRAY_TOKEN=... go test ./...
func TestAgainstARealServer(t *testing.T) {
	url := os.Getenv("XRAY_URL")
	if url == "" {
		t.Skip("set XRAY_URL (and XRAY_TOKEN) to run against a real X-Ray")
	}
	ctx := context.Background()
	c, err := xray.Connect(ctx, url, xray.WithToken(os.Getenv("XRAY_TOKEN")))
	require.NoError(t, err)
	defer c.Close()

	svc := xrayadk.New(c)
	sess := sessionWith(t, "adk-adapter-test", "tester",
		[2]string{"user", "The deploy window for the billing service is Tuesday morning."},
		[2]string{"model", "Understood: billing deploys on Tuesday mornings."})
	require.NoError(t, svc.AddSessionToMemory(ctx, sess))

	res, err := svc.SearchMemory(ctx, &memory.SearchRequest{Query: "billing deploy window", AppName: "adk-adapter-test", UserID: "tester"})
	require.NoError(t, err)
	require.NotEmpty(t, res.Memories)
	assert.Contains(t, res.Memories[0].Content.Parts[0].Text, "Tuesday")

	for _, m := range res.Memories {
		if tags, _ := m.CustomMetadata["scope"].(string); tags == xray.ScopePrivate {
			_ = c.Delete(ctx, m.ID, xray.WithSubject(xrayadk.Subject("adk-adapter-test", "tester")))
		}
	}
}

// The adapter works the same over X-Ray running inside the program: no server,
// no wire, the real store.
func TestInProcessStore_AsADKMemory(t *testing.T) {
	ctx := context.Background()
	store, err := xraymem.New(ctx)
	require.NoError(t, err)
	svc := xrayadk.New(store)

	require.NoError(t, svc.AddSessionToMemory(ctx, sessionWith(t, "shop", "u1",
		[2]string{"user", "My dog is called Biscuit."}, [2]string{"model", "Lovely."})))
	require.NoError(t, svc.AddSessionToMemory(ctx, sessionWith(t, "shop", "u2",
		[2]string{"user", "My dog is called Rex."}, [2]string{"model", "Lovely."})))
	_, err = store.Upsert(ctx, xray.Write{Kind: xray.KindOperationalPattern, Scope: xray.ScopePublic,
		Title: "Dog policy", Content: "Dogs are welcome in the office on Fridays."})
	require.NoError(t, err)

	res, err := svc.SearchMemory(ctx, &memory.SearchRequest{Query: "dog", AppName: "shop", UserID: "u1"})
	require.NoError(t, err)
	var texts []string
	for _, m := range res.Memories {
		texts = append(texts, m.Content.Parts[0].Text)
	}
	joined := strings.Join(texts, "\n")
	assert.Contains(t, joined, "Biscuit")
	assert.Contains(t, joined, "Dogs are welcome")
	assert.NotContains(t, joined, "Rex", "another user's note is not reachable")
}
