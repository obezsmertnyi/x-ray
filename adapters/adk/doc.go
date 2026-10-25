// Package xrayadk gives a Google ADK (Agent Development Kit) agent X-Ray as its
// long-term memory.
//
// Service implements the ADK memory.Service, so it plugs into the runner and
// the stock load_memory and preload_memory tools:
//
//	mem, _ := xray.Connect(ctx, "http://localhost:8083/mcp", xray.WithToken(token))
//	svc := xrayadk.New(mem)
//
//	r, _ := runner.New(runner.Config{
//		AppName: "my-app", Agent: myAgent,
//		SessionService: sessions, MemoryService: svc,
//	})
//
// Call AddSessionToMemory when a session ends (or after each turn, which is
// safe: it updates in place). SaveTool and RefuteTool give the model write
// access, so it can keep a fact across sessions and retire one that turned out
// to be wrong.
//
// Memory is scoped to the pair of app and user: a user's own conversations are
// private to them, and shared X-Ray memory (facts the team or other agents
// wrote) is added to every search unless WithoutShared is given.
package xrayadk
