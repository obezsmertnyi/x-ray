package xray_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ixray "github.com/triageagent-dev/x-ray/v2/internal/xray"
	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

const secret = "not-a-real-token"

// memoryServer starts the real memory MCP server on a test listener, so the SDK
// is checked against the replies it will see in use.
func memoryServer(t *testing.T) string {
	t.Helper()
	h := ixray.NewMCPServer(ixray.NewMemoryStore(), zerolog.Nop(), func() string { return secret }, nil, nil, "", nil, 0, 0, 0)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func connect(t *testing.T, url string) *xray.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := xray.Connect(ctx, url, xray.WithToken(secret))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestConnect_WithoutTheTokenIsRefused(t *testing.T) {
	url := memoryServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := xray.Connect(ctx, url)
	assert.Error(t, err)
}

func TestTools_ListsTheMemoryTools(t *testing.T) {
	c := connect(t, memoryServer(t))
	names, err := c.Tools(context.Background())
	require.NoError(t, err)
	assert.Subset(t, names, []string{"search_memory", "upsert_memory", "refute_memory", "get_memory_context"})
}

func TestMemory_WriteSearchFetchDelete(t *testing.T) {
	ctx := context.Background()
	c := connect(t, memoryServer(t))

	res, err := c.Upsert(ctx, xray.Write{
		Kind: xray.KindStableFact, Scope: xray.ScopePublic, Confidence: "high",
		Title: "Checkout retries on 503", Content: "The payment client retries a 503 three times with jitter.",
		Subject: "checkout", Tags: []string{"payments"},
	})
	require.NoError(t, err)
	assert.Equal(t, "created", res.Status)
	require.NotEmpty(t, res.ID)

	hits, err := c.Search(ctx, "payment retries 503", xray.WithLimit(5))
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, res.ID, hits[0].ID)
	assert.Equal(t, xray.OriginDerived, hits[0].Origin, "the shared secret names nobody, so the write is derived")

	got, err := c.Get(ctx, res.ID)
	require.NoError(t, err)
	assert.Equal(t, "Checkout retries on 503", got.Title)
	assert.Equal(t, []string{"payments"}, got.Tags)

	listed, err := c.List(ctx, xray.WithKind(xray.KindStableFact))
	require.NoError(t, err)
	assert.Len(t, listed, 1)

	sum, err := c.Summary(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Total)
	assert.Equal(t, 1, sum.ByKind[xray.KindStableFact])

	require.NoError(t, c.Delete(ctx, res.ID))
	_, err = c.Get(ctx, res.ID)
	assert.True(t, xray.IsNotFound(err))
}

func TestMemory_RestatedFactMerges(t *testing.T) {
	ctx := context.Background()
	c := connect(t, memoryServer(t))
	w := xray.Write{Kind: xray.KindStableFact, Scope: xray.ScopePublic, Confidence: "high",
		Title: "Deploys freeze on Fridays", Content: "No production deploys are made after noon on Friday.", Subject: "release"}
	first, err := c.Upsert(ctx, w)
	require.NoError(t, err)
	w.Content = "No production deploys are made after noon on a Friday."
	second, err := c.Upsert(ctx, w)
	require.NoError(t, err)
	assert.Equal(t, "merged", second.Status)
	assert.Equal(t, first.ID, second.MergedInto)
}

func TestMemory_RefuteCascadesToCitingItems(t *testing.T) {
	ctx := context.Background()
	c := connect(t, memoryServer(t))
	base, err := c.Upsert(ctx, xray.Write{Kind: xray.KindHypothesis, Scope: xray.ScopePublic,
		Title: "Cache is cold after deploy", Content: "Latency spikes come from a cold cache after each rollout."})
	require.NoError(t, err)
	child, err := c.Upsert(ctx, xray.Write{Kind: xray.KindWatchItem, Scope: xray.ScopePublic,
		Title: "Warm the cache before rollout", Content: "Pre-warm the edge cache ahead of every release.",
		EvidenceRefs: []string{base.ID}})
	require.NoError(t, err)

	out, err := c.Refute(ctx, base.ID, "the spikes predate the cache", []string{"event-1"})
	require.NoError(t, err)
	assert.Equal(t, "stale", out.Status)
	assert.Equal(t, []string{child.ID}, out.Cascaded)

	hits, err := c.Search(ctx, "cache rollout")
	require.NoError(t, err)
	assert.Empty(t, hits, "retired items leave search")

	_, err = c.Refute(ctx, base.ID, "", nil)
	assert.Error(t, err, "a reason is required")
}

func TestMemory_PrivateItemsNeedTheirSubject(t *testing.T) {
	ctx := context.Background()
	c := connect(t, memoryServer(t))
	res, err := c.Upsert(ctx, xray.Write{Kind: xray.KindNote, Scope: xray.ScopePrivate, Subject: "agent-a",
		Title: "My scratch", Content: "working notes for agent a"})
	require.NoError(t, err)

	hits, err := c.Search(ctx, "scratch")
	require.NoError(t, err)
	assert.Empty(t, hits)
	hits, err = c.Search(ctx, "scratch", xray.WithSubject("agent-a"))
	require.NoError(t, err)
	assert.Len(t, hits, 1)

	err = c.Delete(ctx, res.ID, xray.WithSubject("agent-b"))
	var te *xray.ToolError
	require.True(t, errors.As(err, &te))
	assert.Contains(t, te.Message, "forbidden")
	require.NoError(t, c.Delete(ctx, res.ID, xray.WithSubject("agent-a")))
}

func TestMemory_RefusedWriteIsAToolError(t *testing.T) {
	c := connect(t, memoryServer(t))
	_, err := c.Upsert(context.Background(), xray.Write{Kind: "no-such-kind", Scope: xray.ScopePublic, Title: "t", Content: "c"})
	var te *xray.ToolError
	require.True(t, errors.As(err, &te))
	assert.Equal(t, "upsert_memory", te.Tool)

	_, err = c.Upsert(context.Background(), xray.Write{Kind: xray.KindStableFact, Scope: xray.ScopePublic, Title: "t", Content: "c"})
	assert.Error(t, err, "a stable_fact needs confidence high")
}

func TestMemory_SupersedeExpireConsolidate(t *testing.T) {
	ctx := context.Background()
	c := connect(t, memoryServer(t))
	old, err := c.Upsert(ctx, xray.Write{Kind: xray.KindOperationalPattern, Scope: xray.ScopePublic,
		Title: "Queue drains hourly", Content: "The ingest queue drains once an hour."})
	require.NoError(t, err)

	sup, err := c.Supersede(ctx, old.ID, xray.Write{Kind: xray.KindOperationalPattern, Scope: xray.ScopePublic,
		Title: "Queue drains every ten minutes", Content: "The ingest queue now drains every ten minutes."})
	require.NoError(t, err)
	assert.Equal(t, old.ID, sup.Superseded)
	assert.NotEqual(t, old.ID, sup.ID)

	require.NoError(t, c.Expire(ctx, sup.ID))
	_, err = c.Get(ctx, sup.ID)
	assert.NoError(t, err, "an expired item is kept, with status stale")

	rep, err := c.Consolidate(ctx, false)
	require.NoError(t, err)
	assert.False(t, rep.Applied)
}

func TestCall_UnknownToolIsAnError(t *testing.T) {
	c := connect(t, memoryServer(t))
	_, err := c.Call(context.Background(), "no_such_tool", nil)
	assert.Error(t, err)
}
