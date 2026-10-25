# TODO: Async A2A in Voice Sessions

Reference implementation: `../triage-agent/internal/jucce/` — `tools.go` (lines 1715-1843), `session.go` (lines 77-108).

## Overview

Add async Agent-to-Agent (A2A) calls to X-Ray voice sessions. The voice model can delegate work to a remote A2A agent, get a task ID back immediately, and receive the result as a proactive notification injected into the live Gemini session — without blocking the conversation.

## Files to touch

1. **`api/v1alpha1/memorycell_types.go`** — add `RemoteAgent` and `RemoteAgentsConfig` structs; add `RemoteAgents []RemoteAgent` field to `VoiceSessionConfig`
   ```go
   type RemoteAgent struct {
       Name        string      `json:"name"`
       URL         string      `json:"url"`
       Description string      `json:"description,omitempty"`
       Enabled     *bool       `json:"enabled,omitempty"`
       Auth        *AuthConfig `json:"auth,omitempty"`
   }
   type AuthConfig struct {
       Type       string       `json:"type"` // "bearer" or "header"
       SecretRef  MCPSecretRef `json:"secretRef"`
       HeaderName string       `json:"headerName,omitempty"`
   }
   ```

2. **`internal/xray/voice.go`** — add `notifyMu sync.Mutex` to `VoiceSession`; implement `Notify(summary string)` method that uses `TryLock` to avoid stacking and injects a hidden user message via `s.live.SendClientContent()`

3. **`internal/xray/voice_tools.go`** — add `defCallA2AAgentAsync()` tool + `pollA2ATask()` background goroutine; wire into `buildVoiceTools()` (pass session reference for `Notify` callback); add auth HTTP client helper reading from `SecretStore`

4. **`go.mod`** — add `github.com/a2aproject/a2a-go`

## Flow

```
Voice model calls: call_a2a_agent_async(agent_name, message)
  → resolve agent URL + auth from VoiceSessionConfig.RemoteAgents
  → POST to A2A endpoint with Blocking: false
  → if inline result: s.Notify() immediately
  → if task ID: launch pollA2ATask() goroutine (polls every 3s, 10min timeout)
       → on terminal state: s.Notify("[agent X completed: <result>]")
       → Gemini receives hidden user message, continues naturally
```

## Notes

- Remote agents configured in `system_configuration` MemoryCell (`spec.voiceConfig.remoteAgents`)
- Auth token read from k8s Secret via existing `SecretStore` (same pattern as MCP tokens)
- `Notify()` must use `TryLock` — skip if a notification is already in flight
- No changes needed to `cmd/xray/main.go` or Helm chart for MVP (config lives in CR)
- Add a2a-go import; do NOT add the full triage-agent A2A server (inbound side) unless needed
