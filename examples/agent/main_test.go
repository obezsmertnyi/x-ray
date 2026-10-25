package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ixray "github.com/triageagent-dev/x-ray/v2/internal/xray"
	"github.com/triageagent-dev/x-ray/v2/pkg/memory"
	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

const token = "not-a-real-token"

func connect(t *testing.T) *xray.Client {
	t.Helper()
	h := ixray.NewMCPServer(ixray.NewMemoryStore(), zerolog.Nop(), func() string { return token }, nil, nil, "", nil, 0, 0, 0)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := xray.Connect(context.Background(), ts.URL+"/mcp", xray.WithToken(token))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The agent saves a fact in one question and recalls it in the next.
func TestAgent_RemembersAcrossQuestions(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(connect(t), scriptedModel{}, "example-agent")

	first, err := agent.Answer(ctx, "Remember that checkout retries a 503 three times with jitter.")
	require.NoError(t, err)
	assert.Contains(t, first, "saved")

	second, err := agent.Answer(ctx, "How many times does checkout retry a 503?")
	require.NoError(t, err)
	assert.Contains(t, second, "three times with jitter")
	assert.Contains(t, second, "derived", "the shared secret names nobody, so the saved item reads as derived")
}

// scriptedCalls plays a fixed list of tool calls, then answers.
type scriptedCalls struct{ calls [][]Call }

func (s *scriptedCalls) Next(_ context.Context, _ string, history []Turn, _ []Tool) (Turn, error) {
	n := 0
	for _, h := range history {
		if len(h.Calls) > 0 {
			n++
		}
	}
	if n < len(s.calls) {
		return Turn{Calls: s.calls[n]}, nil
	}
	return Turn{Text: history[len(history)-1].Results[0].Text}, nil
}

// What the agent saves cites the items it had recalled, so refuting a fact
// retires the notes drawn from it.
func TestAgent_RefutingAFactRetiresWhatCitedIt(t *testing.T) {
	ctx := context.Background()
	mem := connect(t)

	base, err := mem.Upsert(ctx, xray.Write{Kind: xray.KindHypothesis, Scope: xray.ScopePublic,
		Title: "Cache is cold after deploy", Content: "Latency spikes come from a cold cache after each rollout."})
	require.NoError(t, err)

	save := &scriptedCalls{calls: [][]Call{{{ID: "1", Name: "save_memory", Args: map[string]any{
		"title": "Warm the cache before rollout", "content": "Pre-warm the edge cache ahead of every release.", "kind": "watch_item",
	}}}}}
	_, err = NewAgent(mem, save, "example-agent").Answer(ctx, "cache rollout latency")
	require.NoError(t, err)

	refute := &scriptedCalls{calls: [][]Call{{{ID: "1", Name: "refute_memory", Args: map[string]any{
		"id": base.ID, "reason": "the spikes predate the cache",
	}}}}}
	out, err := NewAgent(mem, refute, "example-agent").Answer(ctx, "was the cache the cause?")
	require.NoError(t, err)
	assert.Contains(t, out, "retired "+base.ID+" and 1 items")

	left, err := mem.Search(ctx, "cache rollout")
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestAgent_AToolRefusalGoesBackToTheModel(t *testing.T) {
	bad := &scriptedCalls{calls: [][]Call{{{ID: "1", Name: "save_memory", Args: map[string]any{
		"title": "t", "content": "c", "kind": "no-such-kind",
	}}}}}
	out, err := NewAgent(connect(t), bad, "example-agent").Answer(context.Background(), "save something")
	require.NoError(t, err)
	assert.Contains(t, out, "error:")
}

// The same agent runs on X-Ray inside its own process, with no server at all.
func TestAgent_RunsOnAnInProcessStore(t *testing.T) {
	ctx := context.Background()
	store, err := memory.New(ctx)
	require.NoError(t, err)
	agent := NewAgent(store, scriptedModel{}, "example-agent")

	_, err = agent.Answer(ctx, "Remember that checkout retries a 503 three times with jitter.")
	require.NoError(t, err)
	got, err := agent.Answer(ctx, "How many times does checkout retry a 503?")
	require.NoError(t, err)
	assert.Contains(t, got, "three times with jitter")
}
