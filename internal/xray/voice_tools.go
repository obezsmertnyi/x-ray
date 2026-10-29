package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"github.com/triageagent-dev/x-ray/v2/internal/headroom"
	"github.com/triageagent-dev/x-ray/v2/internal/version"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
	"google.golang.org/genai"
)

// voiceSearchMinLimit floors the result count for search_memory. Voice mishears
// can tie multiple items at equal fuzzy edit distance; a model-supplied limit of
// 1 would surface only an arbitrary tie-break winner, so we always return a few
// candidates for the model to disambiguate against.
const voiceSearchMinLimit = 3

// nativeKindGloss describes the raw ADR-020 memory kinds by their intrinsic
// meaning — deployment-agnostic, with NO per-deployment category names (a
// deployment may relabel a kind, e.g. stable_fact→"Blog"; that mapping belongs
// in the system prompt, not the tool). Shared by list_memory / search_memory so
// the model knows every valid kind, including the SRE-flavored ones.
const nativeKindGloss = "'active_concern' (live/current issues), " +
	"'operational_pattern' (recurring behaviour, known noise, self-heals), " +
	"'stable_fact' (durable knowledge, findings), " +
	"'system_identity' (systems, services, people, topology), " +
	"'event' (time-bound items, meetings, schedule), " +
	"'note' (general or personal notes), " +
	"'recent_change' (releases, updates), 'watch_item', 'hypothesis'"

// buildVoiceTools returns the memory tools for a voice session.
// enabledTools is an allowlist of tool names from the system_configuration CR;
// if empty, all tools are included.
// username is the human-readable login (display only).
// userHash is the stable, non-spoofable SHA-256[:16] of the SSO ID; it is the
// match key for the writeAllowed admin list (display names are spoofable).
func buildVoiceTools(memory *MemoryStore, syncer *CellSyncer, namespace, username, userHash string, writeAllowed []string, enabledTools []string, headroomLimit int, logger zerolog.Logger, getPinnedIDs func() []string, pendingHangup *atomic.Bool, _ *atomic.Bool, hangup chan struct{}) []voiceToolDef {
	canWrite := userHash != "" && slices.Contains(writeAllowed, userHash)
	all := []voiceToolDef{
		defGetMemory(memory),
		defListMemory(memory, userHash),
		defSearchMemory(memory, userHash, headroomLimit, logger),
		defSaveMemory(memory, syncer, namespace, userHash),
		defEndSession(pendingHangup, hangup),
	}
	// Session-scoped tools require an authenticated user — skip when auth is disabled.
	if userHash != "" {
		all = append(all,
			defRecentMemory(memory, userHash, headroomLimit, logger),
			defFetchMemory(memory, userHash, getPinnedIDs, headroomLimit, logger),
			defDeleteMemory(memory, syncer, namespace, userHash),
		)
	}
	if canWrite {
		all = append(all,
			defUpsertMemory(memory, syncer, namespace, userHash),
		)
	}
	if len(enabledTools) == 0 {
		return all
	}
	allowed := make(map[string]bool, len(enabledTools))
	for _, n := range enabledTools {
		allowed[n] = true
	}
	// These tools always bypass the allowlist: session control, write tools (gated
	// separately by writeAllowedUsers), and session-scoped tools that have no
	// equivalent in the CR config (fetch_memory, recent_memory).
	writeTool := map[string]bool{
		"end_session":   true,
		"upsert_memory": true,
		"delete_memory": true,
		"fetch_memory":  true,
		"recent_memory": true,
	}
	out := make([]voiceToolDef, 0, len(all))
	for _, t := range all {
		if allowed[t.decl.Name] || writeTool[t.decl.Name] {
			out = append(out, t)
		}
	}
	return out
}

func defEndSession(pendingHangup *atomic.Bool, hangup chan struct{}) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "end_session",
			Description: "End the voice session and hang up. Call this when the user says goodbye, thanks and ends the conversation, or explicitly asks to hang up or stop. Before you say goodbye, save this conversation's episodic memory (save_memory, kind=episodic) if your instructions ask for one. Then ALWAYS speak one brief, warm goodbye out loud FIRST, then call this as your last action — it hangs up at once, so say nothing after it. Never end the session silently.",
		},
		handler: func(_ context.Context, _ map[string]any) (string, error) {
			pendingHangup.Store(true)
			// The session hangs up without answering this call (handleToolCall), so
			// the model never reads this; it is only what the browser and trace see.
			return `{"status":"ok"}`, nil
		},
	}
}

func defGetMemory(memory *MemoryStore) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "get_memory",
			Description: "Get an orientation summary of stored memory: item counts by kind.",
		},
		handler: func(_ context.Context, _ map[string]any) (string, error) {
			items := memory.AllItems("")
			counts := map[string]int{}
			for _, it := range items {
				counts[it.Kind]++
			}
			b, _ := json.Marshal(map[string]any{
				"total":   len(items),
				"by_kind": counts,
			})
			return string(b), nil
		},
	}
}

func defListMemory(memory *MemoryStore, userHash string) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "list_memory",
			Description: "List memory items by kind for browsing and explicit list requests (for example: 'list events' or 'show recent concerns'). Pass kind to filter to one category. Do not call this as a mandatory step before answering specific lookup questions — use search_memory for those.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"kind":  {Type: genai.TypeString, Description: "Filter to one kind: " + nativeKindGloss},
					"limit": {Type: genai.TypeInteger, Description: "Max results to return (default 20)"},
				},
			},
		},
		handler: func(ctx context.Context, args map[string]any) (string, error) {
			kind, _ := args["kind"].(string)
			if msg := ValidateSearchKind(kind); msg != "" {
				return errJSON(msg), nil
			}
			limit := 20
			if l, ok := args["limit"].(float64); ok && l > 0 {
				limit = int(l)
			}
			items := memory.List(kind, "", "", limit)
			// A listing of notes (or of everything) includes the caller's own
			// private notes, the same as search_memory does.
			if userHash != "" && (kind == "" || kind == "note") {
				seen := make(map[string]struct{}, len(items))
				for _, it := range items {
					seen[it.ID] = struct{}{}
				}
				for _, n := range memory.ListRecent("note", scopePrivate, userHash, limit) {
					if _, ok := seen[n.ID]; !ok {
						items = append(items, n)
					}
				}
			}
			memory.sharedHit(items, kind)
			type row struct {
				ID      string `json:"id"`
				Kind    string `json:"kind"`
				Scope   string `json:"scope"`
				Title   string `json:"title"`
				Subject string `json:"subject,omitempty"`
			}
			rows := make([]row, 0, len(items))
			for _, it := range items {
				rows = append(rows, row{ID: it.ID, Kind: it.Kind, Scope: it.Scope, Title: it.Title, Subject: it.Subject})
			}
			b, _ := json.Marshal(map[string]any{"count": len(rows), "items": rows})
			return string(b), nil
		},
	}
}

func defSearchMemory(memory *MemoryStore, userHash string, headroomLimit int, logger zerolog.Logger) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "search_memory",
			Description: "Search the knowledge graph by keyword or phrase. Includes shared knowledge and the current user's personal notes. Use this as the default tool whenever the user asks about a specific service, incident, person, topic, or item by name. Always search before saying you don't know something.",
			Parameters: &genai.Schema{
				Type:     genai.TypeObject,
				Required: []string{"query"},
				Properties: map[string]*genai.Schema{
					"query": {Type: genai.TypeString, Description: "Search terms: service or system name, person name, incident, or topic"},
					"kind":  {Type: genai.TypeString, Description: "Optional filter to one kind: " + nativeKindGloss},
					"limit": {Type: genai.TypeInteger, Description: "Max results (default 10)"},
				},
			},
		},
		handler: func(ctx context.Context, args map[string]any) (string, error) {
			query, _ := args["query"].(string)
			kind, _ := args["kind"].(string)
			if msg := ValidateSearchKind(kind); msg != "" {
				return errJSON(msg), nil
			}
			limit := 10
			if l, ok := args["limit"].(float64); ok && l > 0 {
				limit = int(l)
			}
			// Floor the limit: voice mishears ("RAC"→"RAG"/"RAY") can tie two items
			// at equal edit distance, and a limit of 1 would return whichever wins an
			// arbitrary recency/ID tie-break. Returning a few candidates lets the model
			// disambiguate in its spoken answer rather than confidently picking wrong.
			if limit < voiceSearchMinLimit {
				limit = voiceSearchMinLimit
			}
			// Shared search runs across all subjects (empty subject). With subject
			// empty, the store guard (isPrivateItem) excludes private cells, so no
			// other user's notes can surface here (ADR-028/037). Personal notes come
			// only from the userHash-scoped merge below.
			sharedKind := kind
			if sharedKind == "note" {
				sharedKind = ""
			}
			// Search runs lexical first and fuses semantic only when lexical
			// confidence is low (ADR-030) — the keyword-first fast path lives inside.
			items := memory.Search(ctx, query, sharedKind, "", limit)
			// Merge the user's own private notes when authenticated and no shared-only
			// kind filter is in effect. Scoped to subject=userHash, so isolation holds.
			if userHash != "" && (kind == "" || kind == "note") {
				noteLimit := limit - len(items)
				if noteLimit < 3 {
					noteLimit = 3
				}
				// Ranked search scoped to the user: lexical first, semantic fused in
				// when the keywords are not a confident match, so a spoken question
				// finds a note that only shares its meaning.
				notes := memory.Search(ctx, query, "note", userHash, noteLimit)
				seen := make(map[string]struct{}, len(items))
				for _, it := range items {
					seen[it.ID] = struct{}{}
				}
				for _, n := range notes {
					if _, ok := seen[n.ID]; !ok {
						items = append(items, n)
					}
				}
			}
			memory.sharedHit(items, kind)
			hrRows := make([]headroom.Row, 0, len(items))
			for _, it := range items {
				// event_time annotation (docs/todo/graphiti-comparison-refinement.md
				// item #1) goes into Title, not Content: Content is what
				// CompressRows lossy-truncates below, and a truncated tail would
				// cut the annotation off; Title always passes through whole.
				title := it.Title + memstore.EventTimeAnnotation(it.EventDate)
				hrRows = append(hrRows, headroom.Row{ID: it.ID, Kind: it.Kind, Title: title, Content: it.Content})
			}
			// Headroom token compression (ADR-038): lossy-compress long bodies
			// (full content stays retrievable by id) and factor uniform fields.
			// No row dropping — every search candidate reaches the model (voice
			// mishears need them for disambiguation).
			return compressItemRows("search_memory", map[string]any{"query": query}, hrRows, headroomLimit, logger), nil
		},
	}
}

// buildRemoteMCPTools connects to each configured MCP server, fetches its tool list,
// and returns the open clients and a voiceToolDef for each remote tool. Callers must
// close the returned clients when the session ends. Tools from all servers are
// flattened into a single list; name collisions are resolved by prefixing with
// "<server>__<tool>".
func buildRemoteMCPTools(ctx context.Context, servers []MCPServerRef, secrets *SecretStore, namespace string, existing []voiceToolDef, headroomLimit int, logger zerolog.Logger) ([]*mcpclient.Client, []voiceToolDef) {
	var clients []*mcpclient.Client
	var out []voiceToolDef
	seen := map[string]bool{}
	for _, t := range existing {
		seen[t.decl.Name] = true
	}

	for _, srv := range servers {
		// Resolve token from SecretStore at session start so rotation takes effect.
		if srv.TokenSecretName != "" && secrets != nil {
			srv.Token = secrets.Get(namespace, srv.TokenSecretName, srv.TokenSecretKey)
		}
		c, tools, err := fetchMCPTools(ctx, srv, headroomLimit, logger)
		if err != nil {
			logger.Warn().Err(err).Str("mcp_server", srv.Name).Msg("failed to fetch MCP tools")
			continue
		}
		clients = append(clients, c)
		allowed := make(map[string]bool, len(srv.Tools))
		for _, n := range srv.Tools {
			allowed[n] = true
		}
		registered := 0
		for _, t := range tools {
			if len(allowed) > 0 && !allowed[t.decl.Name] {
				continue
			}
			name := t.decl.Name
			if seen[name] {
				name = srv.Name + "__" + name
				t.decl.Name = name
			}
			seen[name] = true
			out = append(out, t)
			registered++
		}
		logger.Info().Str("mcp_server", srv.Name).Int("tools", registered).Msg("registered remote MCP tools")
	}
	return clients, out
}

func fetchMCPTools(ctx context.Context, srv MCPServerRef, headroomLimit int, logger zerolog.Logger) (*mcpclient.Client, []voiceToolDef, error) {
	opts := []transport.StreamableHTTPCOption{}
	if srv.Token != "" {
		opts = append(opts, transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + srv.Token,
		}))
	}
	c, err := mcpclient.NewStreamableHttpClient(srv.URL, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("create client: %w", err)
	}
	if err := c.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start client: %w", err)
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "xray", Version: version.Version},
		},
	}); err != nil {
		c.Close() //nolint:errcheck
		return nil, nil, fmt.Errorf("initialize client: %w", err)
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close() //nolint:errcheck
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}

	out := make([]voiceToolDef, 0, len(res.Tools))
	for _, t := range res.Tools {
		decl := &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
		}
		if t.InputSchema.Type != "" || len(t.InputSchema.Properties) > 0 {
			decl.Parameters = mcpSchemaToGenai(t.InputSchema)
		}
		out = append(out, voiceToolDef{
			decl: decl,
			handler: func(ctx context.Context, args map[string]any) (string, error) {
				req := mcp.CallToolRequest{}
				req.Params.Name = t.Name
				req.Params.Arguments = args
				result, err := c.CallTool(ctx, req)
				if err != nil {
					return "", fmt.Errorf("call %s: %w", t.Name, err)
				}
				// Flatten content blocks into a single string.
				var parts []string
				for _, content := range result.Content {
					if tc, ok := content.(mcp.TextContent); ok {
						parts = append(parts, tc.Text)
					}
				}
				text := strings.Join(parts, "\n")
				return compressToolResult(text, t.Name, headroomLimit, logger), nil
			},
		})
	}
	return c, out, nil
}

// compressToolResult applies the headroom content budget to any voice tool
// result before it reaches the voice context — a single result (e.g. xray-code
// get_code_node returning a whole ADR, README, or source body) can be tens of
// KB. It strips markdown noise and truncates to limit chars (limit<=0 =
// passthrough), and emits the "headroom compression" debug line so the real win
// (bytes/tokens/%) is observable. Used by every result-bearing path: the
// built-in tools, the native recall code tools, and remote MCP tools.
//
// It ALWAYS adopts the crushed form: LossyCompress strips markdown noise even
// when the body fits under the budget (truncated=false), and that strip is the
// dominant per-result saving on this corpus (ADR-038). The earlier code kept the
// original unless truncated, leaking the strip saving on every under-budget
// result.
func compressToolResult(text, toolName string, limit int, logger zerolog.Logger) string {
	if limit <= 0 {
		return text
	}
	crushed, truncated := headroom.LossyCompress(text, limit)
	bytesIn := len(text)
	text = crushed
	bytesOut := len(text)
	saved := bytesIn - bytesOut
	pct := 0.0
	if bytesIn > 0 {
		pct = float64(saved) / float64(bytesIn) * 100
	}
	logger.Debug().
		Str("tool", toolName).
		Bool("truncated", truncated).
		Int("bytes_in", bytesIn).Int("bytes_out", bytesOut).
		Int("bytes_saved", saved).Int("tokens_saved_est", saved/4).
		Str("reduction", strconv.FormatFloat(pct, 'f', 1, 64)+"%").
		Int("limit", limit).
		Msg("headroom compression")
	return text
}

// compressItemRows is the shared {count, items:[{id,kind?,title,content,truncated?}],
// common?} result builder for the memory-cell list tools (search_memory,
// get_pinned_items, get_agent_notes). It lossy-compresses each row's content to
// the budget and factors uniform fields (ADR-038) — NO row dropping, every item
// reaches the model; full content stays retrievable by id. extra is merged into
// the top-level object (e.g. {"query": q} for search). It returns the serialized
// JSON and emits the "headroom compression" debug line measured apples-to-apples
// against the pre-compression serialization. limit<=0 is a passthrough (rows
// serialized as-is, no log).
func compressItemRows(toolName string, extra map[string]any, in []headroom.Row, limit int, logger zerolog.Logger) string {
	type row struct {
		ID        string `json:"id"`
		Kind      string `json:"kind,omitempty"`
		Title     string `json:"title"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated,omitempty"`
	}
	build := func(rows []headroom.Row, constant map[string]string) ([]byte, int) {
		out := make([]row, 0, len(rows))
		truncated := 0
		for _, r := range rows {
			out = append(out, row{ID: r.ID, Kind: r.Kind, Title: r.Title, Content: r.Content, Truncated: r.Truncated})
			if r.Truncated {
				truncated++
			}
		}
		obj := map[string]any{"count": len(out), "items": out}
		for k, v := range extra {
			obj[k] = v
		}
		if len(constant) > 0 {
			obj["common"] = constant
		}
		b, _ := json.Marshal(obj)
		return b, truncated
	}

	if limit <= 0 {
		b, _ := build(in, nil)
		return string(b)
	}

	res := headroom.CompressRows(in, headroom.Config{MaxContentChars: limit})
	b, truncated := build(res.Rows, res.Constant)
	// rawB: pre-compression serialization (full content, no factoring), measured
	// the same way so bytes_in/out are apples-to-apples.
	rawB, _ := build(in, nil)
	saved := len(rawB) - len(b)
	pct := 0.0
	if len(rawB) > 0 {
		pct = float64(saved) / float64(len(rawB)) * 100
	}
	logger.Debug().
		Str("tool", toolName).
		Int("items", len(res.Rows)).Int("truncated", truncated).Bool("factored", len(res.Constant) > 0).
		Int("bytes_in", len(rawB)).Int("bytes_out", len(b)).
		Int("bytes_saved", saved).Int("tokens_saved_est", saved/4).
		Str("reduction", strconv.FormatFloat(pct, 'f', 1, 64)+"%").
		Int("limit", limit).
		Msg("headroom compression")
	return string(b)
}

// mcpSchemaToGenai converts an MCP JSON Schema object into a Gemini Schema.
// Only handles the subset Gemini Live accepts: object/string/integer/number/boolean/array.
func mcpSchemaToGenai(s mcp.ToolInputSchema) *genai.Schema {
	schema := &genai.Schema{
		Type: jsonTypeToGenai(s.Type),
	}
	if len(s.Properties) > 0 {
		schema.Properties = make(map[string]*genai.Schema, len(s.Properties))
		for k, v := range s.Properties {
			prop := &genai.Schema{}
			if pm, ok := v.(map[string]any); ok {
				prop.Type = jsonTypeToGenai(fmt.Sprintf("%v", pm["type"]))
				if desc, ok := pm["description"].(string); ok {
					prop.Description = desc
				}
			}
			schema.Properties[k] = prop
		}
	}
	if len(s.Required) > 0 {
		schema.Required = s.Required
	}
	return schema
}

func jsonTypeToGenai(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "integer":
		return genai.TypeInteger
	case "number":
		return genai.TypeNumber
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	default:
		return genai.TypeObject
	}
}

func defUpsertMemory(memory *MemoryStore, syncer *CellSyncer, namespace, userHash string) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "upsert_memory",
			Description: "Create or update a memory item. To update an existing private note, call get_agent_notes first to obtain the id, then pass it here. Omit id only when creating new. Only available to authorised users.",
			Parameters: &genai.Schema{
				Type:     genai.TypeObject,
				Required: []string{"kind", "scope", "title", "content"},
				Properties: map[string]*genai.Schema{
					"id":         {Type: genai.TypeString, Description: "ID of the item to update. Must come from get_pinned_items or get_agent_notes — never from search_memory (search results may not contain the target item). Omit only when creating a brand new item."},
					"kind":       {Type: genai.TypeString, Description: "One of: active_concern, operational_pattern, stable_fact, system_identity, event, note, recent_change, watch_item, hypothesis"},
					"scope":      {Type: genai.TypeString, Description: "Scope: 'private' (personal note for this user) or 'public' (shared knowledge)"},
					"title":      {Type: genai.TypeString, Description: "Short title for the item"},
					"content":    {Type: genai.TypeString, Description: "Full content in Markdown"},
					"subject":    {Type: genai.TypeString, Description: "Author or subject tag"},
					"confidence": {Type: genai.TypeString, Description: "Confidence level: high, medium, or low"},
				},
			},
		},
		handler: func(ctx context.Context, args map[string]any) (string, error) {
			id, _ := args["id"].(string)
			kind, _ := args["kind"].(string)
			scope, _ := args["scope"].(string)
			title, _ := args["title"].(string)
			content, _ := args["content"].(string)
			subject, _ := args["subject"].(string)
			confidence, _ := args["confidence"].(string)
			if kind == "" || scope == "" || title == "" || content == "" {
				return "", fmt.Errorf("kind, scope, title, and content are required")
			}
			if msg := ValidateWrite(kind, confidence); msg != "" {
				return errJSON(msg), nil
			}
			// Voice is user-scoped: any private write is forced to this user's
			// subject, so a session can never write into another user's space
			// (ADR-028/037).
			if isPrivateScope(scope) && userHash != "" {
				subject = userHash
			}
			result, errMsg := memory.Upsert(voiceWriteCtx(ctx, userHash), id, kind, scope, title, content, subject, confidence, "", nil, nil)
			if errMsg != "" {
				return fmt.Sprintf(`{"error":%q}`, errMsg), nil
			}
			if syncer != nil && result.Item != nil {
				it := result.Item
				go func() { syncer.Persist(context.Background(), it, namespace) }() //nolint:errcheck
			}
			b, _ := json.Marshal(map[string]any{"id": result.ID, "status": result.Status, "kind": kind, "title": title})
			return string(b), nil
		},
	}
}

func defDeleteMemory(memory *MemoryStore, syncer *CellSyncer, namespace, userHash string) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "delete_memory",
			Description: "Delete a personal note by ID. Only private notes may be deleted via voice. To find the ID: call list_memory(kind=\"note\") first, then pass the matching ID here. Do not search with search_memory before deleting — list_memory is the correct tool to retrieve your own note IDs.",
			Parameters: &genai.Schema{
				Type:     genai.TypeObject,
				Required: []string{"id"},
				Properties: map[string]*genai.Schema{
					"id": {Type: genai.TypeString, Description: "ID of the note to delete (get it from get_agent_notes)"},
				},
			},
		},
		handler: func(ctx context.Context, args map[string]any) (string, error) {
			id, _ := args["id"].(string)
			if id == "" {
				return "", fmt.Errorf("id is required")
			}
			item := memory.Get(id)
			if item == nil {
				return fmt.Sprintf(`{"error":"not found","id":%q}`, id), nil
			}
			if !isPrivateScope(item.Scope) {
				return fmt.Sprintf(`{"error":"forbidden","reason":"only private notes may be deleted via voice","scope":%q}`, item.Scope), nil
			}
			if item.Subject != userHash {
				return `{"error":"forbidden","reason":"note belongs to another user"}`, nil
			}
			memory.Delete(id)
			if syncer != nil {
				go func() { syncer.Remove(context.Background(), id, namespace) }() //nolint:errcheck
			}
			b, _ := json.Marshal(map[string]any{"status": "deleted", "id": id})
			return string(b), nil
		},
	}
}

// defFetchMemory is the exact-retrieval tool (ADR-051): full content of one or
// more known items by id, in the order given. Replaces get_pinned_items and
// get_agent_notes. When the model supplies ids, fetch exactly those (this is
// the case for "get the full content of that item I found earlier"). When
// ids is omitted, default to the user's currently-pinned X-Ray items — this
// preserves get_pinned_items' zero-arg "what have I pinned" behavior under
// the unified name. get_agent_notes' job (browse your own private notes) is
// now list_memory(kind="note", subject=<your own>) — no separate tool needed.
func defFetchMemory(memory *MemoryStore, userHash string, getPinnedIDs func() []string, headroomLimit int, logger zerolog.Logger) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "fetch_memory",
			Description: "Fetch the full content of one or more known memory items by ID, in the order given — use this when you already have the id(s), e.g. from a prior list_memory/search_memory result. Omit ids to instead return the items the user has pinned in the X-Ray view ('what I pinned', 'what I'm looking at').",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"ids": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}, Description: "Item IDs to fetch. Omit to return the user's pinned items instead."},
				},
			},
		},
		handler: func(_ context.Context, args map[string]any) (string, error) {
			var ids []string
			if raw, ok := args["ids"].([]any); ok {
				for _, v := range raw {
					if s, ok := v.(string); ok && s != "" {
						ids = append(ids, s)
					}
				}
			}
			pinnedFallback := false
			if len(ids) == 0 {
				ids = getPinnedIDs()
				pinnedFallback = true
			}
			if len(ids) == 0 {
				b, _ := json.Marshal(map[string]any{"count": 0, "items": []any{}})
				return string(b), nil
			}
			rows := make([]headroom.Row, 0, len(ids))
			visible := make([]string, 0, len(ids))
			visibleKinds := make([]string, 0, len(ids))
			for _, id := range ids {
				it := memory.Get(id)
				if it == nil {
					continue
				}
				// A private item is only visible to its own owner — mirrors the
				// subject==userHash gate in delete_memory. Applies whether ids came
				// from the model or from the (client-supplied, unauthenticated)
				// pinned-ids set.
				if isPrivateScope(it.Scope) && it.Subject != userHash {
					continue
				}
				rows = append(rows, headroom.Row{ID: it.ID, Kind: it.Kind, Title: it.Title, Content: it.Content})
				visible = append(visible, it.ID)
				visibleKinds = append(visibleKinds, it.Kind)
			}
			// Only publish hits for items the caller may actually see, so the SSE
			// highlight channel can't confirm the existence of another user's
			// private note by ID.
			if pinnedFallback {
				memory.PublishHit(visible, "", visibleKinds)
			}
			// Headroom token compression (ADR-038): lossy-compress long bodies,
			// factor uniform fields. No row dropping — full content by id.
			return compressItemRows("fetch_memory", nil, rows, headroomLimit, logger), nil
		},
	}
}

// defRecentMemory recalls the most recent episodic memories (kind=episodic) —
// the agent's record of what happened in past sessions, newest-first by creation
// time (ADR-020). It is the read side of save_memory(kind=episodic): the
// model calls it early in a session to remember the last conversation. Bodies are
// headroom-compressed; createdAt is surfaced so the model can reason about
// recency. No row dropping.
func defRecentMemory(memory *MemoryStore, userHash string, headroomLimit int, logger zerolog.Logger) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "recent_memory",
			Description: "Recall your most recent session memories (what happened in past conversations), newest first. Call this EARLY in a session — especially when the user resumes, says 'where were we', or asks about something you may have discussed before — so you continue from the last session instead of starting cold.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"limit": {Type: genai.TypeInteger, Description: "How many recent memories to return (default 5, max 20)"},
				},
			},
		},
		handler: func(_ context.Context, args map[string]any) (string, error) {
			limit := 5
			if l, ok := args["limit"].(float64); ok && l > 0 {
				limit = int(l)
			}
			if limit > 20 {
				limit = 20
			}
			items := memory.ListRecent("episodic", scopePrivate, userHash, limit)
			type row struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Content   string `json:"content"`
				CreatedAt string `json:"created_at"`
				Truncated bool   `json:"truncated,omitempty"`
			}
			rows := make([]row, 0, len(items))
			for _, it := range items {
				content := it.Content
				truncated := false
				if headroomLimit > 0 {
					content, truncated = headroom.LossyCompress(it.Content, headroomLimit)
				}
				rows = append(rows, row{
					ID:        it.ID,
					Title:     it.Title,
					Content:   content,
					CreatedAt: it.CreatedAt.UTC().Format(time.RFC3339),
					Truncated: truncated,
				})
			}
			b, _ := json.Marshal(map[string]any{"count": len(rows), "items": rows})
			return string(b), nil
		},
	}
}

func defSaveMemory(memory *MemoryStore, syncer *CellSyncer, namespace, userHash string) voiceToolDef {
	return voiceToolDef{
		decl: &genai.FunctionDeclaration{
			Name:        "save_memory",
			Description: "Save a personal memory for the current user, to recall in future sessions. kind=note (default) for a standing observation, concern, or hypothesis. kind=episodic for a memory of THIS conversation — what happened, what the user asked, what was decided — saved at the end of a session so the next session can recall it.",
			Parameters: &genai.Schema{
				Type:     genai.TypeObject,
				Required: []string{"title", "content"},
				Properties: map[string]*genai.Schema{
					"title":   {Type: genai.TypeString, Description: "Short title for the note"},
					"content": {Type: genai.TypeString, Description: "Full content in Markdown"},
					"kind":    {Type: genai.TypeString, Description: "note (default) for a standing observation, or episodic for a memory of this session"},
				},
			},
		},
		handler: func(ctx context.Context, args map[string]any) (string, error) {
			title, _ := args["title"].(string)
			content, _ := args["content"].(string)
			if title == "" || content == "" {
				return "", fmt.Errorf("title and content are required")
			}
			// kind defaults to "note"; "episodic" marks a session memory (ADR-020).
			// Restrict to the two the model is told about — anything else falls back
			// to note, so a mishear can't create an arbitrary kind.
			kind := "note"
			if k, _ := args["kind"].(string); k == "episodic" {
				kind = "episodic"
			}
			// Voice sessions are always user-scoped: notes are private to the caller
			// (scope=private, subject=userHash). ADR-037.
			result, errMsg := memory.Upsert(voiceWriteCtx(ctx, userHash), "", kind, scopePrivate, title, content, userHash, "medium", "", nil, nil)
			if errMsg != "" {
				return fmt.Sprintf(`{"error":%q}`, errMsg), nil
			}
			if syncer != nil && result.Item != nil {
				it := result.Item
				go func() { syncer.Persist(context.Background(), it, namespace) }() //nolint:errcheck
			}
			b, _ := json.Marshal(map[string]any{"id": result.ID, "status": result.Status})
			return string(b), nil
		},
	}
}

// voiceWriteCtx marks a voice tool's write as human when the session belongs to
// an identified user: the person is speaking, the model only holds the pen.
func voiceWriteCtx(ctx context.Context, userHash string) context.Context {
	if userHash == "" {
		return ctx
	}
	return withWriteOrigin(ctx, OriginHuman)
}
