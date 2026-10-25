# ADR-013: Pluggable Skill SDK for Voice Sub-Agents

**Status:** Accepted  

## Context

X-Ray's voice session tools (`voice_tools.go`) are hardcoded: `get_memory`, `search_memory`, `get_memory_items`, `note_observation`, `end_session`. Adding a new capability (k8s investigation, GitHub lookups, etc.) requires modifying the core server and redeploying.

The triage-agent project (`github.com/triage-agent-dev/triage-agent`) solved this with a pluggable skill system built on the Go ADK (`google.golang.org/adk`). Skills are Go packages that self-register via `init()`, contribute tool definitions, and optionally run as ephemeral ADK sub-agents for multi-step tasks. Skills are enabled at runtime via a `JucceSkill` CRD — no redeployment needed.

## Decision

Port the Jucce skill SDK to X-Ray as a first-class feature. The Go ADK (`google.golang.org/adk v0.4.0`) is the sub-agent runtime. Skills are pluggable, CRD-driven, and isolated from core voice session logic.

## Architecture

### Skill interface (`internal/xray/skill.go`)

```go
type Skill interface {
    Name() string
    Tools(cfg SkillConfig) ([]ToolDef, error)
}

// Optional — skill injects guidance into the system prompt at session start.
type SkillWithInstructions interface {
    Skill
    Instructions() string
}

// SkillConfig is passed to Tools() at load time.
type SkillConfig struct {
    Model     string            // optional model override from X-RaySkill.spec.model
    Namespace string
    Params    map[string]string // from X-RaySkill.spec.params
}

// ToolDef is what each skill returns — mirrors jucce.ToolDef.
type ToolDef struct {
    Decl    *genai.FunctionDeclaration
    Handler func(ctx context.Context, args map[string]any) (any, error)
}
```

Skills self-register via `init()`:
```go
// skills/memory/skill.go
func init() { xray.RegisterSkill(&MemorySkill{}) }
```

No hardcoded skill list anywhere — adding a skill = blank-import the package in `cmd/xray/main.go`.

### Ephemeral sub-agent runner (`internal/xray/skillagent/runner.go`)

For multi-step tasks that would overflow the Gemini Live context window, the voice session delegates to a short-lived ADK agent:

```go
func Run(ctx context.Context, skillModel string, defaultModel *GatewayModel,
    toolDefs []ToolDef, task string, log zerolog.Logger) (string, error)
```

Internally: `llmagent.New` + `runner.New` + `session.InMemoryService()` → iterate `r.Run()` → return final text response. The caller (voice tool handler) gets a spoken summary back synchronously.

### Voice tools: `delegate_to_skill` + `check_skill_progress`

Two new voice tools added to `voice_tools.go`:

- `delegate_to_skill(skill, task, [cluster])` — launches `runSkillAgent()` in a goroutine, returns `task_id` immediately. Voice model tells the user "I'm on it" and polls.
- `check_skill_progress(task_id)` — returns `{status: "running"|"done"|"error", result}`. Voice model polls until done, then speaks the result.

This async pattern is required because ADK sub-agent execution (~5–30s) would block the Gemini Live turn.

### `VoiceManager` skill lifecycle

```
X-RaySkill CR created/changed
  → SkillReconciler → VoiceManager.ReloadSkills(crs)
    → buildSkillTools() — calls skill.Tools(cfg) per enabled CR
      → toolDefs + instructions cached in VoiceManager
        → injected into every new voice session at session start
```

`ReloadSkills` is hot — no session restart needed. Already-running sessions keep their tool set; new sessions pick up the updated one.

### `X-RaySkill` CRD (`api/v1alpha1/xray_skill_types.go`)

| Field | Type | Purpose |
|---|---|---|
| `spec.skill` | string | Registered Go skill name (e.g. `"k8s"`, `"memory"`) |
| `spec.enabled` | bool | Toggle without deleting the CR |
| `spec.model` | string | Optional LLM override for this skill's sub-agent |
| `spec.tools` | []string | Allowlist — empty means all tools |
| `spec.policy` | ToolPolicy | Allow/deny by tool name or verb prefix |
| `spec.params` | map[string]string | Skill-specific config (e.g. `readOnly: "true"`) |
| `status.ready` | bool | Set by reconciler after successful load |
| `status.toolCount` | int | Number of tools contributed |

### Voice blocklist

Large-output tools (e.g. `k8s_get_pod_logs`) are blocked from direct Gemini Live calls — they overflow the 32k context window. The system prompt instructs the voice model to use `delegate_to_skill` for those operations instead. The blocklist is maintained in `skill.go` as `voiceBlocklist`.

## First skills

| Skill name | Package | Tools |
|---|---|---|
| `memory` | `skills/memory/` | `memory_search`, `memory_get`, `memory_list`, `memory_note` (thin wrappers over MemoryStore — same as current hardcoded tools but skill-packaged) |
| `k8s` | `skills/k8s/` | Port from `triage-agent/internal/jucce/skills/k8s/` — 14 tools covering CRUD, logs, events, API discovery |

The existing hardcoded memory tools in `voice_tools.go` remain as a fallback when no `memory` skill CR is installed. Long-term they can be replaced by the skill entirely.

## Implementation plan

1. `go get google.golang.org/adk@v0.4.0`
2. `internal/xray/skill.go` — `Skill` interface, `SkillConfig`, `ToolDef`, `RegisterSkill`, registry
3. `internal/xray/skillagent/runner.go` — ephemeral ADK runner (port from triage)
4. `voice_tools.go` — add `defDelegateToSkill` + `defCheckSkillProgress`, task store on `VoiceSession`
5. `voice.go` — add `skillDefs []toolDef` + `ReloadSkills` to `VoiceManager`; inject skill tools at session start
6. `api/v1alpha1/xray_skill_types.go` — CRD types
7. `internal/xray/skill_reconciler.go` — watches `X-RaySkill` CRs, calls `ReloadSkills`
8. `skills/memory/skill.go` — first skill; blank-imported in `cmd/xray/main.go`

## Reference

- `triage-agent/internal/jucce/skill.go` — Skill interface, registry, policy
- `triage-agent/internal/jucce/manager.go` — ReloadSkills, runSkillAgent
- `triage-agent/internal/jucce/skillagent/runner.go` — ephemeral ADK runner
- `triage-agent/api/v1alpha1/jucceskill_types.go` — CRD schema
- `triage-agent/internal/jucce/skills/k8s/skill.go` — example skill
