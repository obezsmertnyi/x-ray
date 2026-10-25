# Contributing

## ADRs and Todos

Architecture decisions live in `docs/adr/`, work items in `docs/todo/`. Both dirs have a `README.md` status table — keep it up to date.

**Before starting any implementation**, read the ADR decision section top to bottom and verify each requirement. Do not rely on memory — grep and read the code.

**When implementing an ADR:**
1. Update `docs/adr/<N>-<slug>.md` header:
   ```
   **Status:** Implemented
   **Date:** <date>
   **Commit:** <short-hash>
   **Todo:** [<slug>](../todo/<slug>.md)
   ```
2. If a matching `docs/todo/` file exists, add at the top:
   ```
   **Status:** Done — ADR-<N>, commit <short-hash>
   ```
3. Update `docs/adr/README.md`: set status, commit, link the Todo column, and add test function names in the Tests column.
4. Update `docs/todo/README.md`: set status, commit ref, and test function names in the Tests column.
5. Include all of the above in a single commit alongside the code change and changelog entry.

## Prerequisites

- Go 1.27.3+
- Docker or [ko](https://ko.build) for container builds
- A Kubernetes cluster (optional — runs without k8s in local dev)

## Local development

```bash
# Run the server locally (auth off, :8080)
make run

# Or build the binary with the web UI embedded (make ui builds it first; Node 22)
make build
./bin/xray
```

## Voice

Set one of:

```bash
# Vertex AI
./bin/xray --gcp-project=<project> --gcp-location=europe-west4

# Google AI API
./bin/xray --gemini-api-key=<key>
```

## Semantic memory

Set one of:

```bash
# Vertex AI
./bin/xray --gcp-project=<project> --embedding-model=text-embedding-005

# OpenAI-compatible gateway
./bin/xray --embedding-endpoint=http://localhost:8000 --embedding-model=bge-small-en-v1.5
```

Leave both unset to use keyword search only.

## Code quality

```bash
make lint    # gofmt + go vet
make test    # go test ./...
```

## Pull requests

- One logical change per PR
- `go test ./...` must pass
- `make lint` must pass clean

## Project structure

See [CLAUDE.md](CLAUDE.md) for a full architecture reference.
