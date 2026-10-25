# Repository Guidelines

## Project Structure & Module Organization
- `cmd/xray/`: Go entrypoint (the memory server and `xray graph`).
- `internal/xray/`: core runtime modules (HTTP server, memory, voice, auth, SSE hub, MCP server).
- `internal/vectorstore/`, `internal/config/`, `internal/observability/`: supporting packages.
- `api/v1/` + `config/crd/`: Kubernetes CRD types and generated manifests.
- `charts/xray/` and `deploy/`: Helm chart and deployment manifests.
- `docs/adr/`: architecture decisions; read before changing core behavior.

## Build, Test, and Development Commands
- `make run`: run backend locally on `:8080`.
- `make build`: compile `bin/xray` (API only; the web UI is a separate service).
- `make test`: run Go test suite (`go test ./...`).
- `make lint`: enforce formatting (`gofmt -l`) and run `go vet`.
- `make fmt`: apply `gofmt -w` across Go files.

## Coding Style & Naming Conventions
- Go: standard `gofmt` formatting (tabs), idiomatic package layout, lowercase package names.
- Keep modules focused by domain (`voice.go`, `memory.go`, `mcpserver.go`) and place tests next to code (`*_test.go`).

## Testing Guidelines
- Primary tests are Go unit/integration tests under `internal/**` and `internal/vectorstore/**`.
- Name tests with clear behavior scope (`TestXxx` with subtests where useful).
- Run `make test` before opening a PR; add tests for changed behavior, especially API, memory, and voice flows.

## Commit & Pull Request Guidelines
- Follow conventional commit prefixes used in history: `feat:`, `fix:`, `docs:`, `chore:`.
- Keep commits scoped and descriptive (e.g., `fix: initialize MCP client before tool listing`).
- PRs should include: purpose, key changes, test evidence (command output summary), and linked issue/ticket.
- Frontend changes do not belong in this repo; the web UI is a separate service.
- Code change + CHANGELOG.md entry + ADR/todo updates must all land in a single commit — never split them.

## ADR & Todo Completion
When an ADR is fully implemented, do all of the following in the same commit as the code:
1. Update the ADR header: `**Status:** Implemented`, `**Commit:** <short-hash>`, `**Todo:** [<slug>](../todo/<slug>.md)` (if a todo file exists).
2. If a matching `docs/todo/` file exists, add `**Status:** Done — ADR-<N>, commit <short-hash>` at the top.
3. Update `docs/adr/README.md`: set status, commit, link the Todo column, and add test function names in the Tests column.
4. Update `docs/todo/README.md`: set status, commit ref, and test function names in the Tests column.

## Security & Configuration Tips
- Never commit secrets; use env vars or Kubernetes Secrets (`--mobile-users-secret`, `--mcp-token-secret`).
- For local auth/tool work, use temporary tokens via environment variables, not hardcoded values.
