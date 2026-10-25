// Command example runs a Google ADK agent whose long-term memory is X-Ray.
//
//	export GOOGLE_API_KEY=...                     # Gemini
//	go run ./example                              # X-Ray runs inside this process
//
//	export XRAY_URL=http://localhost:8083/mcp     # or use a shared X-Ray
//	export XRAY_TOKEN=...
//	go run ./example
//
// Without XRAY_URL the agent embeds X-Ray's memory store and keeps it in
// adk-memory.json. With it, the agent connects to a running X-Ray over MCP. The
// agent code is the same either way.
//
// Tell the agent something, quit, start it again with the same USER_ID, and ask
// about it: the second run finds it through X-Ray, not through the session.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/loadmemorytool"
	"google.golang.org/adk/tool/preloadmemorytool"
	"google.golang.org/genai"

	"github.com/triageagent-dev/x-ray/v2/pkg/memory"
	"github.com/triageagent-dev/x-ray/v2/pkg/xray"

	xrayadk "github.com/triageagent-dev/x-ray/adapters/adk"
)

const appName = "memory_assistant"

func main() {
	ctx := context.Background()
	userID := getenv("USER_ID", "demo-user")

	// 1. X-Ray: in this process, or a running one over MCP.
	var mem xrayadk.Memory
	if url := os.Getenv("XRAY_URL"); url != "" {
		c, err := xray.Connect(ctx, url, xray.WithToken(os.Getenv("XRAY_TOKEN")))
		if err != nil {
			log.Fatalf("connect to X-Ray: %v", err)
		}
		defer c.Close()
		mem = c
	} else {
		s, err := memory.New(ctx, memory.WithFile(getenv("MEMORY_FILE", "adk-memory.json")))
		if err != nil {
			log.Fatalf("open memory: %v", err)
		}
		defer s.Close()
		mem = s
	}

	// 2. Wrap it as an ADK memory service, and give the model write tools.
	memoryService := xrayadk.New(mem)
	save, err := xrayadk.SaveTool(mem)
	if err != nil {
		log.Fatal(err)
	}
	refute, err := xrayadk.RefuteTool(mem)
	if err != nil {
		log.Fatal(err)
	}

	// 3. The agent: preload recalls memory before each turn, load_memory lets the
	// model search for more, save_memory and refute_memory let it write.
	llm, err := gemini.NewModel(ctx, getenv("GEMINI_MODEL", "gemini-3.1-flash-lite"), &genai.ClientConfig{
		APIKey: os.Getenv("GOOGLE_API_KEY"),
	})
	if err != nil {
		log.Fatalf("model: %v", err)
	}
	assistant, err := llmagent.New(llmagent.Config{
		Name:        "memory_assistant",
		Model:       llm,
		Description: "An assistant that remembers across sessions.",
		Instruction: "You are a helpful assistant with long-term memory. " +
			"Relevant memory is preloaded for each request; call load_memory to look for more. " +
			"Save durable facts the user tells you with save_memory. " +
			"A memory marked origin=human outranks one marked derived. " +
			"If a memory turns out to be wrong, call refute_memory with the reason.",
		Tools: []tool.Tool{preloadmemorytool.New(), loadmemorytool.New(), save, refute},
	})
	if err != nil {
		log.Fatal(err)
	}

	// 4. The runner takes the memory service.
	sessions := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          assistant,
		SessionService: sessions,
		MemoryService:  memoryService,
	})
	if err != nil {
		log.Fatal(err)
	}
	created, err := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID})
	if err != nil {
		log.Fatal(err)
	}
	sessionID := created.Session.ID()

	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nyou -> ")
		line, err := in.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
		fmt.Print("agent -> ")
		msg := genai.NewContentFromText(line, genai.RoleUser)
		for ev, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{}) {
			if err != nil {
				fmt.Printf("\nerror: %v\n", err)
				continue
			}
			if ev.LLMResponse.Content == nil || ev.LLMResponse.Partial {
				continue
			}
			for _, p := range ev.LLMResponse.Content.Parts {
				fmt.Print(p.Text)
			}
		}
		fmt.Println()

		// 5. Write the conversation into X-Ray. It updates in place, so doing it
		// after every turn is safe and loses nothing if the process stops.
		cur, err := sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
		if err == nil {
			if err := memoryService.AddSessionToMemory(ctx, cur.Session); err != nil {
				log.Printf("save to X-Ray: %v", err)
			}
		}
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
