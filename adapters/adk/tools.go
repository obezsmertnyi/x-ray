package xrayadk

import (
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

// SaveArgs are the arguments of the save_memory tool.
type SaveArgs struct {
	Title   string `json:"title" jsonschema:"short title of what to remember"`
	Content string `json:"content" jsonschema:"the fact, pattern or decision, in a sentence or two"`
	Kind    string `json:"kind,omitempty" jsonschema:"stable_fact, operational_pattern, hypothesis, watch_item or note (default note)"`
	Shared  bool   `json:"shared,omitempty" jsonschema:"true to share it with other agents and people; default is private to this user"`
}

// SaveResult is the reply of the save_memory tool.
type SaveResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// SaveTool lets the model keep something across sessions. What it saves is
// private to the user unless it sets shared. The server records it as derived:
// a model's write never replaces what a person wrote.
func SaveTool(mem Memory) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "save_memory",
		Description: "Save something worth keeping across sessions: a durable fact, a pattern, a decision. Do not save chatter.",
	}, func(ctx agent.ToolContext, a SaveArgs) (SaveResult, error) {
		kind := a.Kind
		if kind == "" {
			kind = xray.KindNote
		}
		scope := xray.ScopePrivate
		if a.Shared {
			scope = xray.ScopePublic
		}
		w := xray.Write{
			Kind: kind, Scope: scope, Subject: Subject(ctx.AppName(), ctx.UserID()),
			Title: a.Title, Content: a.Content,
		}
		if kind == xray.KindStableFact {
			w.Confidence = "high" // the server requires it for a stable fact
		}
		res, err := mem.Upsert(ctx, w)
		if err != nil {
			return SaveResult{}, err
		}
		return SaveResult{ID: res.ID, Status: res.Status}, nil
	})
}

// RefuteArgs are the arguments of the refute_memory tool.
type RefuteArgs struct {
	ID     string `json:"id" jsonschema:"the id of the memory that is wrong"`
	Reason string `json:"reason" jsonschema:"why it is wrong"`
}

// RefuteResult is the reply of the refute_memory tool.
type RefuteResult struct {
	Status   string   `json:"status"`
	Cascaded []string `json:"cascaded"`
}

// RefuteTool lets the model retire a memory it finds to be wrong, with the
// reason. Items that cite it are retired with it.
func RefuteTool(mem Memory) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "refute_memory",
		Description: "Mark a remembered item as wrong, with the reason. Use expire only for items that are merely out of date.",
	}, func(ctx agent.ToolContext, a RefuteArgs) (RefuteResult, error) {
		if a.ID == "" || a.Reason == "" {
			return RefuteResult{}, fmt.Errorf("id and reason are required: %w", functiontool.ErrInvalidArgument)
		}
		res, err := mem.Refute(ctx, a.ID, a.Reason, nil, xray.WithSubject(Subject(ctx.AppName(), ctx.UserID())))
		if err != nil {
			return RefuteResult{}, err
		}
		return RefuteResult{Status: res.Status, Cascaded: res.Cascaded}, nil
	})
}
