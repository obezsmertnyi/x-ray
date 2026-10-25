# xray-graph

Serves pre-built code-graph snapshots over MCP, HTTP and a browser UI.

The chart deploys **snapshot mode** (`serve-snapshots`): one graph and index per
map, loaded lazily from `<label>.graph.gob.gz`, routed to per call, evicted
after an idle TTL. That is the mode meant for immutable distribution — static
binary, no repository checkout, no C toolchain. Live mode (`serve`) parses
source on the node and is a workstation mode; it is not templated here.

## Install

```sh
helm install xray-graph charts/xray-graph \
  --set embeddings.endpoint=http://llama-embeddings:8090 \
  --set snapshots.image.repository=ghcr.io/you/your-maps \
  --set snapshots.image.tag=2026-09-15
```

## The two things that decide whether it works

**The maps.** The server exits at startup if the snapshot directory holds no
`*.graph.gob.gz` — an empty volume is a CrashLoopBackOff, not an empty server.
After that the directory is watched (inotify): a snapshot dropped onto the
volume is registered after a short debounce, a rewritten one is re-registered,
a removed one is dropped, all without a restart. The exception is an NFS-backed
volume, where inotify does not fire and a restart is still needed.

**The fingerprint.** `embeddings.dims` and `embeddings.model` must match what
built the snapshots. A dims mismatch is a hard rejection (map skipped, logged);
a model-name mismatch only warns. See [Embeddings](#embeddings).

## Where the maps come from

Every OCI mount in this chart is an **image volume** (`volumes[].image`), so
the chart needs Kubernetes 1.33 or newer with the `ImageVolume` feature on (it
is on by default from 1.33). On an older cluster the chart refuses to render;
on a cluster with the gate off the pod stays Pending with an image-volume
error. There is no other way this chart gets maps into a pod.

| `snapshots.source` | What happens |
|---|---|
| `image` (default) | The maps image is mounted read-only as an image volume, `snapshots.image.path` selected by subPath. No copy, no initContainer, no shell or `cp` in the maps image, no size limit, and the mount is immutable. Versioned with the release. Read-only serving only. |
| `pvc` | Mounts a PersistentVolumeClaim, existing or chart-created. The writable choice when worked copies and notes must outlive the pod. Can be seeded on first start — see below. |
| `ephemeral` | An emptyDir seeded from the maps image (`snapshots.image`) at **every** pod start by the seed initContainer. The writable choice without a PVC: the maps stay versioned with the image, and `update_code_graph`'s worked copies and `remember`'s notes live exactly as long as the pod — a restart, reschedule or rollout starts from an empty volume and reseeds it. `snapshots.ephemeral.sizeLimit` caps it, `snapshots.ephemeral.medium: Memory` makes it a tmpfs. |

Build a maps image from snapshots produced by `xray graph snapshot` — it needs
nothing but the files:

```dockerfile
FROM scratch
COPY *.graph.gob.gz /maps/
```

```sh
docker build -t ghcr.io/you/xray-maps:2026-09-15 . && docker push ghcr.io/you/xray-maps:2026-09-15
```

An image volume is resolved when the pod is created, so a moved tag is only
picked up on pod recreation. Pin a tag per release.

## Encrypted maps

A snapshot is gzip over gob: anyone who can pull the maps image can `strings`
every node's text, and for a catalog map that is people's names and roles.
Build such maps encrypted and give only the server the key:

```sh
xray graph keygen -out snapshot.key                   # prints the age1… recipient
xray graph servicemap -in catalog.json -label harness -snapshot-recipient age1…
kubectl create secret generic xray-graph-snapshot-key --from-file=identity=snapshot.key
```

```yaml
snapshots:
  encryption:
    enabled: true
    secretName: xray-graph-snapshot-key   # key "identity" by default
```

The maps image is built exactly as before — the encrypted file has the same
name, and the seed initContainer copies it like any other. The identity is
mounted at `snapshots.encryption.mountPath` (mode 0400) and the server reads
it from `$XRAY_SNAPSHOT_KEY`. A directory may mix encrypted and plaintext maps;
an encrypted one the key does not open is skipped with a warning naming the
file, and the rest serve. The key reads, the recipient writes: with
`server.writable=true` a map that arrived encrypted is re-encrypted on every
flush (never written back as plaintext), while plaintext maps and the
`remember` notes map the server creates stay plaintext. To encrypt those too,
set `XRAY_SNAPSHOT_RECIPIENT` (via `extraEnv`) to the identity's `age1…`
public key; then every flush is encrypted and an existing volume migrates on
its first write.

This protects the files — on the volume, in the registry. The server itself
answers searches in the clear, so who can reach `/mcp` is still the boundary.

## Seeding a fresh volume

With `snapshots.source=pvc` the claim starts empty, and an empty directory is a
crashloop. `snapshots.seed` fills it at pod start from OCI images built exactly
like the maps image above, mounted as image volumes and copied by a busybox
initContainer (`snapshots.seed.image`):

```sh
helm upgrade --install xray-graph charts/xray-graph \
  --set snapshots.source=pvc --set server.writable=true \
  --set snapshots.seed.enabled=true \
  --set snapshots.seed.images[0].reference=ghcr.io/you/xray-maps:2026-09-15
```

The seed copies only files the volume does not already hold. That is the point:
the volume is where `update_code_graph`'s worked copies and `remember`'s notes
are flushed, and a redeploy must not erase them. `snapshots.seed.overwrite=true`
replaces them — use it only when the seed is the source of truth.
`kubectl logs <pod> -c seed` prints one line per file copied or skipped.

Because the seed is an image, a large code graph seeds as easily as a small
catalog. Once the volume holds a map, the watcher above picks up anything you
copy in later, seed or not.

With `snapshots.source=ephemeral` the maps image is seeded whether or not
`snapshots.seed.enabled` is set — the emptyDir is empty at every start and has
to be filled from somewhere. Enabling `seed` there adds the listed images on
top of the maps image; an empty list is refused. `overwrite` has nothing to
act on (the volume is always fresh) and is harmless.

## Embeddings

Every search embeds the query once, and that vector has to land in the space
the snapshots were built in. `embeddings.model`, `embeddings.dims` and
`embeddings.maxInputChars` are that fingerprint, and every map served must
share it. `dims` is the hard check: the server truncates client-side
(Matryoshka; llama.cpp always returns nomic's native 768) and a map built at
another dims is rejected at load. The model name only warns, because llama.cpp
serves whatever GGUF it loaded regardless of the label.

Two ways to get the embedder:

| | |
|---|---|
| `embeddings.endpoint` (default) | Any OpenAI-compatible `/v1/embeddings` server reachable from the pod. Dialed lazily: the pod starts and reports healthy without it, and only searches fail until it is up. |
| `embeddings.sidecar.enabled=true` | A llama.cpp server running `--embeddings` in the pod, from an image **you supply** with the GGUF baked in — the chart names no registry and has no default image. Rendered as a native sidecar: started first, and its `/health` startupProbe gates the seed and the server, so the first search never races the model load. `embeddings.endpoint` is ignored; the server is pointed at `localhost:<port>`. |

Build the sidecar image from any llama.cpp server base — two lines:

```dockerfile
FROM <a llama.cpp server image>
COPY nomic-embed-text-v1.5.Q8_0.gguf /models/model.gguf
```

```sh
--set embeddings.sidecar.enabled=true \
--set embeddings.sidecar.image=<your image>
```

The GGUF path inside the image is `embeddings.sidecar.modelPath`
(`/models/model.gguf` by default), passed as `--model`.

The sidecar's resources default to 500m CPU / 512Mi requested, 2 CPU / 1Gi
limit. Memory is the mmap'd ~140MB GGUF plus llama.cpp;
CPU is the throughput lever, and a guaranteed slice matters — under node
contention a query embed measured 52ms against 4ms on an uncontended core.
The chart refuses the sidecar with `embeddings.dims` other than 256, the
truncation every shipped snapshot was built at; `embeddings.sidecar.allowOtherDims=true`
is the escape hatch for snapshots you built yourself at another dims.

A full writable install — PVC seeded from a maps image you built with the
Dockerfile example above (here, one small catalog map) and the sidecar for
embeddings:

```sh
helm upgrade --install xray-graph charts/xray-graph \
  --set snapshots.source=pvc --set server.writable=true \
  --set snapshots.seed.enabled=true \
  --set snapshots.seed.images[0].reference=ghcr.io/you/xray-graph-maps:harness \
  --set embeddings.sidecar.enabled=true \
  --set embeddings.sidecar.image=<your image>
```

## Writable mode

`server.writable=true` lets `update_code_graph` edit a worked copy and flush it
back to the `.gob.gz` on eviction and shutdown, and registers `remember`. It
needs a volume the server can write to — `snapshots.source=pvc` or
`snapshots.source=ephemeral`; with `image` the mount is a read-only image and
nothing can flush — and is single-replica: on a PVC each pod flushes its own
copy and replicas overwrite each other, on an emptyDir each replica keeps its
own notes and `recall` answers from whichever pod took the call. The chart
refuses the other combinations at render time.

Which volume decides what survives a pod restart:

| | worked copies and notes |
|---|---|
| `pvc` | kept — the seed copies only files the claim does not already hold |
| `ephemeral` | lost — the emptyDir starts empty and is reseeded from the maps image |

A pair of installs from the same maps image. Read-only, the image mounted
straight in:

```sh
helm upgrade --install xray-graph charts/xray-graph \
  --set snapshots.image.repository=ghcr.io/you/xray-maps \
  --set snapshots.image.tag=2026-09-15
```

Writable on ephemeral storage, the same image seeded into an emptyDir at every
start, with the encryption Secret so encrypted maps in it open (see
[Encrypted maps](#encrypted-maps)):

```sh
helm upgrade --install xray-graph charts/xray-graph \
  --set snapshots.source=ephemeral --set server.writable=true \
  --set snapshots.image.repository=ghcr.io/you/xray-maps \
  --set snapshots.image.tag=2026-09-15 \
  --set snapshots.encryption.enabled=true \
  --set snapshots.encryption.secretName=xray-graph-snapshot-key
```

Encrypted maps copy as bytes, so the seed needs no key; only the server mounts
the identity. Under `server.writable=true` a map that arrived encrypted is
re-encrypted on every flush and the notes map the server creates stays
plaintext — and on `ephemeral` both are discarded with the pod anyway.

## Memory

`remember` writes notes into `<snapshots.mountPath>/<server.memoryMap>.graph.gob.gz`
(default `/maps/session.graph.gob.gz`), created on first use and flushed after
every note, so notes survive a pod restart on a PVC — and do not on
`snapshots.source=ephemeral`, where the map lives as long as the pod. The tool
is registered only under `server.writable=true`; `server.memoryMap=""`
disables it. Read the notes back with
`search_graph map=session kind=Memory order=at:desc`.

## Endpoints

| Path | |
|---|---|
| `/mcp` | MCP over HTTP — the search and graph tools |
| `/ui` | browser graph view |
| `/search`, `/graph`, `/node`, `/projects`, `/status` | JSON, what the UI calls |
| `/metrics` | search-quality counters as **JSON**, not Prometheus text — a ServiceMonitor will not parse it |
| `/healthz` | what the probes use |

All of them serve the whole graph, and with `server.writable=true` `/mcp` also
writes. By default none asks for a credential, so the network is the boundary
and an ingress publishes everything.

## Authentication

`server.auth.enabled=true` requires a bearer token on every route but
`/healthz`, read from a Secret mounted as a file:

```bash
openssl rand -hex 32 > token
kubectl create secret generic xray-graph-auth --from-file=token=token
helm upgrade --install xray-graph oci://ghcr.io/triageagent-dev/charts/xray-graph \
  --reuse-values --set server.auth.enabled=true --set server.auth.secretName=xray-graph-auth
```

MCP clients send `Authorization: Bearer <token>`. The browser UI takes the token
once as `/ui?token=<token>`, keeps it in an HttpOnly cookie and drops it from the
address bar. Browser calls from another origin are refused whether or not auth
is on; allow one with `extraArgs: ["-allowed-origins=https://…"]`.

## RAM

A loaded map holds its graph, vector cache and HNSW in memory; the small routing
vector stays resident after eviction. `server.graphTTLMinutes` and
`resources.limits.memory` are the same dial — the TTL decides how many maps are
resident at once.
