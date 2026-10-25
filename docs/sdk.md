# Adding X-Ray to an agent

There are two ways to give an agent X-Ray, and the same agent code runs on both.

- **Embedded.** The memory store and the code-graph engine run inside your agent's process, as the Go packages `pkg/memory` and `pkg/codegraph`. There is no server and no MCP. The only network call is to the embedding endpoint you choose.
- **Connected.** The agent talks to an X-Ray that is already running, shared with other agents and people, through the Go SDK `pkg/xray` or any MCP client.

`*memory.Store` and `*xray.Client` both implement `xray.Memory`. `*codegraph.Graph` and `*xray.Client` both implement `xray.CodeGraph`. Write the agent against those interfaces and move from one to the other by changing the line that creates it.

An agent uses X-Ray in three places.

1. **Before it answers**, it searches memory for the question and puts what it finds in the prompt.
2. **While it answers**, it has tools to search more, save a fact and refute one that is wrong.
3. **After it answers**, what it learned is written back, linked to the items it drew on.

## Embedded

```go
store, err := memory.New(ctx,
	memory.WithEmbeddings("http://localhost:8090", "nomic", 256), // semantic search
	memory.WithFile("agent-memory.json"),                         // survive a restart
	memory.WithOrigin(xray.OriginDerived),                        // who the agent's writes count as
)
if err != nil { return err }
defer store.Close()

// 1. Recall before answering: shared memory, then the agent's own private notes.
shared, _ := store.Search(ctx, question, xray.WithLimit(5))
own, _ := store.Search(ctx, question, xray.WithSubject("my-agent"), xray.WithLimit(5))
prompt := systemPrompt + render(append(shared, own...)) // show each item's id and origin

// 2. Save what is worth keeping, citing what the model had in front of it.
store.Upsert(ctx, xray.Write{
	Kind: xray.KindOperationalPattern, Scope: xray.ScopePublic, Subject: "my-agent",
	Title: "Checkout retries on 503", Content: "Three times, with jitter.",
	EvidenceRefs: idsOf(recalled),
})

// 3. Retire a fact that proved wrong; whatever cited it goes with it.
store.Refute(ctx, wrongID, "the spikes predate the cache", nil, xray.WithSubject("my-agent"))
```

The store is the one the server runs. Typed items, a keyword and trigram index, an HNSW index when you give it embeddings, merging of a restated fact on write, recall shaping, and provenance. Two behaviours matter to an agent author:

- **Origin.** Every item carries who wrote it. A store opened with `WithOrigin(xray.OriginHuman)` holds what a person told the agent, and a derived write never replaces such an item. Show the origin to the model and tell it that human items outrank derived ones.
- **Evidence.** An item saved with `EvidenceRefs` is retired when any item it cites is refuted. Passing the ids the model was shown costs nothing and makes corrections propagate.

### Code graph in the agent

A coding agent can hold the graph of the code it works on and keep it current as it edits.

```go
emb, _ := codegraph.NewEmbeddings(ctx, "http://localhost:8090", "nomic", 256, 8192)
g, err := codegraph.Build(ctx, emb, codegraph.Repo{Label: "myapp", Path: "./myapp"})

hits, _ := g.SearchGraph(ctx, "where do we retry payments?", xray.Limit(5))
node, _ := g.Node(ctx, hits.Results[0].QN)                       // body, calls, called_by, imports
impact, _ := g.Impact(ctx, []string{hits.Results[0].QN}, 2, 50)  // what a change reaches
risk, _ := g.Risk(ctx, hits.Results[0].QN, 0, 0)                 // LOW, MEDIUM or HIGH

g.UpdateGraph(ctx, "./myapp/pay/retry.go") // after the agent edits a file; only changed nodes are re-embedded
g.Save(ctx, "myapp.graph.gob.gz")          // codegraph.Load reads it back with no parsing
```

`Build` and `UpdateGraph` parse with Tree-sitter, so they need `CGO_ENABLED=1` and a C compiler. A build without cgo can `Load` a snapshot made elsewhere and search it, and `Build` returns `codegraph.ErrNoParser`. A graph made with `xray graph snapshot`, or by an earlier run, loads if its embeddings match the fingerprint recorded in it: the model, the vector size and the input cap. `g.Handler()` returns the graph as an MCP `http.Handler` if the agent should offer it to others.

### A complete loop

`examples/agent` is the three steps above as a runnable agent, with the memory tools described to a model as JSON Schema, a provider-neutral `Model` interface to fill in for your LLM, and a scripted model so it runs with no API key:

```bash
go run ./examples/agent                                   # X-Ray inside this process
XRAY_URL=http://localhost:8083/mcp XRAY_TOKEN=... go run ./examples/agent   # a shared X-Ray
```

With no `XRAY_URL` it opens `memory.New` with a file, and what it learns survives a restart. With one it connects.

## Connected

```go
c, err := xray.Connect(ctx, "http://localhost:8083/mcp", xray.WithToken(token))
if err != nil { return err }
defer c.Close()
```

`c` has the same methods as the embedded store, so the code above runs unchanged. Add `c.SearchGraph`, `c.Node`, `c.Impact` and the rest when the server also serves maps. A refusal by the server comes back as `*xray.ToolError`, and `xray.IsNotFound` tests for a missing item. The credential decides how a write is classed: an OAuth token that names a person records it as `human`, the shared secret as `derived`, and the SDK has no way to claim otherwise. `Call` reaches any tool the typed methods do not cover.

## Google ADK

The adapter, `github.com/triageagent-dev/x-ray/adapters/adk`, implements the ADK `memory.Service` on top of either, so X-Ray plugs into the runner and the stock `preload_memory` and `load_memory` tools. It is a separate Go module, so the ADK dependency stays out of the library.

```go
store, _ := memory.New(ctx, memory.WithFile("agent-memory.json")) // or xray.Connect(...)
memoryService := xrayadk.New(store)
save, _ := xrayadk.SaveTool(store)
refute, _ := xrayadk.RefuteTool(store)

assistant, _ := llmagent.New(llmagent.Config{
	Name: "memory_assistant", Model: llm,
	Instruction: "You have long-term memory. Save durable facts with save_memory; " +
		"call refute_memory when a memory is wrong.",
	Tools: []tool.Tool{preloadmemorytool.New(), loadmemorytool.New(), save, refute},
})

r, _ := runner.New(runner.Config{
	AppName: "my-app", Agent: assistant,
	SessionService: sessions, MemoryService: memoryService,
})

// After a turn, or when the session ends:
memoryService.AddSessionToMemory(ctx, session)
```

How it maps:

| ADK | X-Ray |
|---|---|
| `AddSessionToMemory` | One private note per exchange (a user turn and the reply), subject `app/user`, with a stable id, so adding the same session again updates in place |
| `SearchMemory` | A search under the user's subject, then shared memory (`WithoutShared` leaves it out), de-duplicated, with `origin`, `kind` and `scope` in the entry's metadata |
| `SaveTool` | An upsert, private to the user unless the model sets `shared` |
| `RefuteTool` | A refute under the user's subject |

Another user's notes are never returned, because a private item needs its owner's subject. The full runnable agent, with Gemini, is `adapters/adk/example`; with no `XRAY_URL` it embeds X-Ray and keeps its memory in `adk-memory.json`:

```bash
export GOOGLE_API_KEY=...
cd adapters/adk && go run ./example
```

Tell it something, quit, run it again with the same `USER_ID` and ask about it. The second run finds the answer through X-Ray, not through the session.

## Other frameworks

The pattern is the same for any framework with a memory or retriever hook: implement it with `Search` and `Upsert`, give the model the tools, and write sessions back. The two interfaces, `xray.Memory` and `xray.CodeGraph`, are all an adapter needs to accept.
