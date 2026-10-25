package mcpserver

// The remember and forget tools: an agent writes one note into the agent-notes
// map at runtime, and removes one.
//
// A voice session ends when the user hangs up and the next starts blank, so
// "what did we discuss last time" had no answer even though the server had
// answered every question in it. The note is embedded and persisted at once
// (engine.Registry.Remember flushes the snapshot), and read back with the
// ordinary search tools: kind=Memory selects the notes, order=at:desc puts the
// newest first. Registered on both the live and the read-only surfaces when
// Deps.Memory and Deps.MemoryMap are set; a server with no memory backing does
// not offer the tools rather than offering ones that fail.
//
// Notes also die. A memory that never forgets fills with every session's
// running notes, and a search for "what did we decide" ranks March beside
// today with nothing to tell them apart. So every note carries an expiry — the
// ttl argument, or the server's default when the note names none — and the
// engine sweeps expired notes out (engine.Registry sweeps on every load of a
// notes map and on a timer). forget is the same removal on demand, by
// qualified name, for a note that was wrong or is no longer wanted.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// memoryKind is the node kind every note carries — the name a caller selects
// the set by. Mirrors engine's constant; the reply note spells it out so a
// caller never has to know it in advance.
const memoryKind = "Memory"

// rememberDesc is the tool's description; a function because it names the
// agent-notes map and the default lifetime, which are configuration, not
// constants.
func rememberDesc(d Deps) string {
	return fmt.Sprintf("Write one note into your own notes (the %q map) so a later conversation can recall it: what the user told you, asked, decided, a preference, where the call ended. The note is embedded at once and persisted. Read notes back with recall — it lists the newest first, or ranks them by a query. Keep a note to one or two sentences that will make sense out of context; name people, maps and symbols in full. A note ABOUT a person or record that a map holds (a student, a service) must carry about=<the record's title or handle> in attrs, e.g. attrs=about=octocat42, so recall with that handle as the query returns it beside the map's own result; such a note supplements the record and never replaces it — the map stays the source of the facts. A note expires after the server default of %s unless ttl says otherwise (ttl=30d, ttl=never); forget removes one early by the qn this call returns.", d.MemoryMap, FormatTTL(d.memoryTTL()))
}

// forgetDesc is the forget tool's description.
func forgetDesc(d Deps) string {
	return fmt.Sprintf("Remove one of your own notes from the %q map by its qualified name — the qn remember returned, or the one each note carries in a recall reply. Only a Memory node in the notes map can be forgotten; a code or catalog node is refused. Use it for a note that was wrong, superseded, or that the user asked you to drop; notes also expire on their own after their ttl.", d.MemoryMap)
}

// memoryTTL is the default lifetime the description states: Deps.MemoryTTL
// when set, otherwise the package default the engine also falls back to.
func (d Deps) memoryTTL() time.Duration {
	if d.MemoryTTL == 0 {
		return DefaultMemoryTTL
	}
	return d.MemoryTTL
}

func registerRemember(s *server.MCPServer, d Deps) {
	if d.Memory == nil || d.MemoryMap == "" {
		return
	}
	s.AddTool(mcp.NewTool("remember",
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    boolPtr(false),
			DestructiveHint: boolPtr(false),
			IdempotentHint:  boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		}),
		mcp.WithDescription(rememberDesc(d)),
		mcp.WithString("text", mcp.Description("The note itself, one or two sentences that will make sense out of context"), mcp.Required()),
		mcp.WithString("title", mcp.Description("Optional short name for the note; becomes part of its qualified name")),
		mcp.WithString("attrs", mcp.Description("Optional facets as \"key=value,key=value\" (e.g. session=abc123,topic=team). Each becomes an attribute a later search can select with attr=key=value; the server always adds at=<time> and expires=<time>. about=<title or handle> names the map record the note is about (e.g. about=octocat42) and puts the note first in a recall for that name.")),
		mcp.WithString("ttl", mcp.Description("Optional lifetime: a duration such as 7d, 24h or 90m, or never. Omitted means the server default; after it the note is swept out of the map.")),
	), logged(d, "remember", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		attrs, err := parseAttrs(argStr(args, "attrs"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ttl, err := ParseTTL(argStr(args, "ttl"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out, err := remember(ctx, d, argStr(args, "text"), argStr(args, "title"), attrs, ttl)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(out)
	}))

	s.AddTool(mcp.NewTool("forget",
		mcp.WithToolAnnotation(mcp.ToolAnnotation{
			ReadOnlyHint:    boolPtr(false),
			DestructiveHint: boolPtr(true),
			IdempotentHint:  boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		}),
		mcp.WithDescription(forgetDesc(d)),
		mcp.WithString("qn", mcp.Description("Qualified name of the note to remove, e.g. session//memory/20260915T190000.000Z-title.md::20260915T190000.000Z-title"), mcp.Required()),
	), logged(d, "forget", func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := forget(ctx, d, argStr(req.GetArguments(), "qn"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(out)
	}))
}

// remember is the shared body of the MCP tool and RememberDirect: refuses an
// empty note here (before anything is written) and shapes the reply.
//
// There is deliberately NO map argument. The note always lands in
// Deps.MemoryMap, the agent-notes map. An earlier version let the caller name
// the map, which let an agent write prose into a code or catalog map — memory
// poisoning: a later search of that map would rank the agent's own sentence
// beside the parsed truth, and nothing marks which is which. The engine
// refuses a non-notes map as well (engine.Registry.Remember), so removing the
// argument here is the first of two locks, not the only one.
func remember(ctx context.Context, d Deps, text, title string, attrs map[string]string, ttl time.Duration) (map[string]any, error) {
	if d.Memory == nil || d.MemoryMap == "" {
		return nil, fmt.Errorf("memory is not enabled on this server")
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("text is required")
	}
	r, err := d.Memory.Remember(ctx, d.MemoryMap, Note{Text: text, Title: title, Attrs: attrs, TTL: ttl})
	if err != nil {
		return nil, err
	}
	return rememberResult(r), nil
}

// forget is the shared body of the MCP tool and ForgetDirect. Like remember it
// names no map: the qn must be a note in Deps.MemoryMap, and the engine
// refuses any other node (engine.Registry.Forget).
func forget(ctx context.Context, d Deps, qn string) (map[string]any, error) {
	if d.Memory == nil || d.MemoryMap == "" {
		return nil, fmt.Errorf("memory is not enabled on this server")
	}
	qn = strings.TrimSpace(qn)
	if qn == "" {
		return nil, fmt.Errorf("qn is required: the qualified name remember returned or a kind=Memory search hit carries")
	}
	f, err := d.Memory.Forget(ctx, d.MemoryMap, qn)
	if err != nil {
		return nil, err
	}
	return forgetResult(f), nil
}

// parseAttrs reads "key=value,key=value". Each pair follows the attr filter's
// own rule (a pair with no "=" or an empty key is an error, not a silent skip),
// so a note cannot lose a facet the caller meant to attach.
func parseAttrs(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("attrs must be \"key=value,key=value\", got %q", pair)
		}
		out[k] = v
	}
	return out, nil
}
