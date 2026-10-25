// Command agent shows how to give an agent X-Ray as its memory through the Go
// SDK. It is provider-neutral: the model is an interface, and the example runs
// with a scripted model so it works with no API key. To use a real model,
// implement Model for your provider and pass it to NewAgent.
//
//	go run ./examples/agent                                  # X-Ray in this process
//	XRAY_URL=http://localhost:8083/mcp XRAY_TOKEN=... go run ./examples/agent
//
// With no XRAY_URL the agent runs X-Ray's memory store inside its own process and
// keeps it in agent-memory.json, so what it learns survives a restart. With
// XRAY_URL it uses a shared X-Ray over MCP. The agent code is the same for both,
// because the in-process store and the remote client implement xray.Memory.
//
// The agent does three things with X-Ray:
//
//  1. Recall. Before it answers, it searches memory for the question and puts
//     what it finds in the system prompt, each item with its id and origin.
//  2. Tools. It hands the model three memory tools, so the model decides what
//     is worth saving and what is wrong.
//  3. Evidence. Anything the model saves is linked to the items that were in
//     front of it, so refuting a fact later retires what was drawn from it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/triageagent-dev/x-ray/v2/pkg/memory"
	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mem Memory
	if url := os.Getenv("XRAY_URL"); url != "" {
		c, err := xray.Connect(ctx, url, xray.WithToken(os.Getenv("XRAY_TOKEN")), xray.WithClientInfo("example-agent", "1"))
		if err != nil {
			log.Fatal(err)
		}
		defer c.Close()
		mem = c
	} else {
		s, err := memory.New(ctx, memory.WithFile(getenv("MEMORY_FILE", "agent-memory.json")))
		if err != nil {
			log.Fatal(err)
		}
		defer s.Close()
		mem = s
	}

	agent := NewAgent(mem, &scriptedModel{}, "example-agent")
	for _, q := range []string{
		"Remember that checkout retries a 503 three times with jitter.",
		"How many times does checkout retry a 503?",
	} {
		fmt.Printf("> %s\n", q)
		answer, err := agent.Answer(ctx, q)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\n\n", answer)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Memory is what the agent needs from X-Ray. *xray.Client satisfies it, and a
// test can supply a fake.
type Memory interface {
	Search(ctx context.Context, query string, opts ...xray.QueryOption) ([]xray.Item, error)
	Upsert(ctx context.Context, w xray.Write) (*xray.WriteResult, error)
	Refute(ctx context.Context, id, reason string, evidence []string, opts ...xray.QueryOption) (*xray.RefuteResult, error)
}

// Agent answers questions with a model, backed by X-Ray memory.
type Agent struct {
	mem     Memory
	model   Model
	subject string // names this agent to X-Ray; private memory is scoped to it
}

// NewAgent returns an agent whose private memory is scoped to subject.
func NewAgent(mem Memory, model Model, subject string) *Agent {
	return &Agent{mem: mem, model: model, subject: subject}
}

// Answer recalls memory for the question, runs the model with the memory tools,
// and returns its final text.
func (a *Agent) Answer(ctx context.Context, question string) (string, error) {
	// 1. Recall. Search without a subject sees shared memory; with the agent's
	// own subject it also sees what the agent keeps private.
	shared, err := a.mem.Search(ctx, question, xray.WithLimit(5))
	if err != nil {
		return "", err
	}
	own, err := a.mem.Search(ctx, question, xray.WithSubject(a.subject), xray.WithLimit(5))
	if err != nil {
		return "", err
	}
	recalled := dedupe(append(shared, own...))

	system := "You are an assistant with a long-term memory.\n" + renderMemory(recalled)

	// 2. Tools, with the ids in front of the model as the evidence for any save.
	tools := a.tools(ids(recalled))

	// 3. The loop: the model asks for tools, the agent runs them, until it answers.
	var history []Turn
	history = append(history, Turn{Text: question})
	for step := 0; step < 6; step++ {
		turn, err := a.model.Next(ctx, system, history, tools)
		if err != nil {
			return "", err
		}
		if len(turn.Calls) == 0 {
			return turn.Text, nil
		}
		history = append(history, turn)
		results := Turn{}
		for _, call := range turn.Calls {
			out, err := runTool(ctx, tools, call)
			if err != nil {
				out = "error: " + err.Error() // the model sees the refusal and can correct itself
			}
			results.Results = append(results.Results, Result{CallID: call.ID, Text: out})
		}
		history = append(history, results)
	}
	return "", fmt.Errorf("the model did not finish in 6 steps")
}

// tools are the memory tools offered to the model. evidence is the set of ids
// that were recalled for this question.
func (a *Agent) tools(evidence []string) []Tool {
	return []Tool{
		{
			Name:        "search_memory",
			Description: "Search long-term memory by meaning. Use it when the recalled memory in the system prompt is not enough.",
			Schema:      objectSchema(map[string]any{"query": strSchema("what to look for")}, "query"),
			Run: func(ctx context.Context, args map[string]any) (string, error) {
				items, err := a.mem.Search(ctx, str(args, "query"), xray.WithSubject(a.subject), xray.WithLimit(5))
				if err != nil {
					return "", err
				}
				return renderMemory(items), nil
			},
		},
		{
			Name:        "save_memory",
			Description: "Save something worth keeping across sessions: a durable fact, a pattern, a decision. Not chatter.",
			Schema: objectSchema(map[string]any{
				"title":   strSchema("short title"),
				"content": strSchema("the fact, in a sentence or two"),
				"kind":    strSchema("stable_fact, operational_pattern, hypothesis, watch_item or note"),
			}, "title", "content"),
			Run: func(ctx context.Context, args map[string]any) (string, error) {
				kind := str(args, "kind")
				if kind == "" {
					kind = xray.KindNote
				}
				w := xray.Write{
					Kind: kind, Scope: xray.ScopePublic, Subject: a.subject,
					Title: str(args, "title"), Content: str(args, "content"),
					EvidenceRefs: evidence,
				}
				if kind == xray.KindStableFact {
					w.Confidence = "high" // the server requires it for a stable fact
				}
				res, err := a.mem.Upsert(ctx, w)
				if err != nil {
					return "", err
				}
				if res.Status == "merged" {
					return "already known; merged into " + res.MergedInto, nil
				}
				return "saved " + res.ID, nil
			},
		},
		{
			Name:        "refute_memory",
			Description: "Mark a remembered item as wrong, with the reason. Items drawn from it are retired too.",
			Schema: objectSchema(map[string]any{
				"id":     strSchema("the id of the wrong item"),
				"reason": strSchema("why it is wrong"),
			}, "id", "reason"),
			Run: func(ctx context.Context, args map[string]any) (string, error) {
				res, err := a.mem.Refute(ctx, str(args, "id"), str(args, "reason"), nil, xray.WithSubject(a.subject))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("retired %s and %d items drawn from it", res.ID, len(res.Cascaded)), nil
			},
		},
	}
}

func runTool(ctx context.Context, tools []Tool, call Call) (string, error) {
	for _, t := range tools {
		if t.Name == call.Name {
			return t.Run(ctx, call.Args)
		}
	}
	return "", fmt.Errorf("no tool %q", call.Name)
}

// renderMemory writes items as text for a prompt. The origin matters to a
// reader: an item a person wrote outranks one a model wrote.
func renderMemory(items []xray.Item) string {
	if len(items) == 0 {
		return "Recalled memory: nothing relevant.\n"
	}
	var b strings.Builder
	b.WriteString("Recalled memory (a human-written item outranks a derived one):\n")
	for _, it := range items {
		origin := it.Origin
		if origin == "" {
			origin = xray.OriginDerived
		}
		fmt.Fprintf(&b, "- [%s, %s] %s: %s\n", it.ID, origin, it.Title, it.Content)
	}
	return b.String()
}

func dedupe(items []xray.Item) []xray.Item {
	seen := map[string]bool{}
	var out []xray.Item
	for _, it := range items {
		if !seen[it.ID] {
			seen[it.ID] = true
			out = append(out, it)
		}
	}
	return out
}

func ids(items []xray.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func str(args map[string]any, k string) string { s, _ := args[k].(string); return s }

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}
