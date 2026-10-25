package main

import (
	"context"
	"strings"
)

// Tool is a function the model may call. Schema is JSON Schema for its
// arguments, which is what every provider's tool-use API takes.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(ctx context.Context, args map[string]any) (string, error)
}

// Call is the model asking for a tool.
type Call struct {
	ID   string
	Name string
	Args map[string]any
}

// Result is a tool's output, returned to the model.
type Result struct {
	CallID string
	Text   string
}

// Turn is one entry in the conversation: text, tool calls the model made, or
// the results the agent ran for it.
type Turn struct {
	Text    string
	Calls   []Call
	Results []Result
}

// Model is the one method an LLM provider must supply. Map system, history and
// tools onto the provider's chat request, and its reply back onto a Turn: text,
// or the tool calls it made.
type Model interface {
	Next(ctx context.Context, system string, history []Turn, tools []Tool) (Turn, error)
}

// scriptedModel stands in for an LLM so the example runs offline. It saves a
// fact when asked to remember one, and otherwise answers from the memory in its
// system prompt.
type scriptedModel struct{}

func (scriptedModel) Next(_ context.Context, system string, history []Turn, _ []Tool) (Turn, error) {
	question := history[0].Text
	last := history[len(history)-1]
	if len(last.Results) > 0 {
		return Turn{Text: "Noted: " + last.Results[0].Text}, nil
	}
	if rest, ok := strings.CutPrefix(question, "Remember that "); ok {
		return Turn{Calls: []Call{{ID: "1", Name: "save_memory", Args: map[string]any{
			"title":   "Checkout retry policy",
			"content": strings.TrimSuffix(rest, "."),
			"kind":    "operational_pattern",
		}}}}, nil
	}
	if i := strings.Index(system, "- ["); i >= 0 {
		return Turn{Text: "From memory: " + strings.TrimSpace(system[i:])}, nil
	}
	return Turn{Text: "I have nothing about that in memory."}, nil
}
