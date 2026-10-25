package xray

import (
	"context"
	"crypto/sha1" //nolint:gosec
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/internal/vectorstore"
)

// MapRegistrar registers/de-registers a mental-map graph carried in a MemoryCell
// as an in-memory snapshot. Satisfied by *recall/engine.Registry. Optional on the
// CellSyncer — nil when the server runs without a recall registry (keyword mode,
// or --mental-maps-dir unset).
type MapRegistrar interface {
	AddSnapshotBytes(ctx context.Context, b []byte) error
	RemoveSnapshotByLabel(label string)
}

// graphDataKey is the Data-blob key under which a mental-map cell carries its
// gzipped-gob graph snapshot. The bytes are the same <label>.graph.gob.gz an OCI
// image-volume would ship — just delivered inside the cell instead.
const graphDataKey = "graph"

// CellSyncer seeds the MemoryStore HNSW index from MemoryCell CRs
// and watches for changes.
type CellSyncer struct {
	client.Client
	memory *MemoryStore
	mapReg MapRegistrar // optional; registers graph-carrying cells as in-memory maps
	logger zerolog.Logger

	// pullSecret names a dockerconfigjson Secret in the cell's namespace that
	// holds the credentials for pulling a graphRef artifact ("" = none).
	// graphFetch is the puller; nil uses the registry client.
	pullSecret string
	graphFetch GraphFetcher

	// mapHashes records the content hash of the gob last registered under each map
	// label, so a reconcile of an unchanged graph-carrying cell skips the expensive
	// re-register (which re-synthesizes the routing vector — an embedder call). The
	// startup reconcile burst replays each cell several times; without this guard
	// every replay re-embeds. Keyed by map label.
	mapMu     sync.Mutex
	mapHashes map[string]string

	// ids maps a cell's resource name to its id, so a delete (which carries only
	// the name) can be applied to the indexes; guarded by idsMu.
	idsMu sync.Mutex
	ids   map[string]string
}

func NewCellSyncer(c client.Client, memory *MemoryStore, logger zerolog.Logger) *CellSyncer {
	return &CellSyncer{
		Client: c,
		memory: memory,
		logger: logger.With().Str("component", "cellsync").Logger(),
		ids:    make(map[string]string),
	}
}

// SetMapRegistrar wires the recall registry so graph-carrying cells (Data["graph"])
// are loaded as searchable in-memory maps. No-op transport when nil.
func (s *CellSyncer) SetMapRegistrar(r MapRegistrar) { s.mapReg = r }

// SetRegistryPullSecret names the Secret whose registry credentials are used to
// pull graphRef artifacts.
func (s *CellSyncer) SetRegistryPullSecret(name string) { s.pullSecret = name }

// SeedAll lists all MemoryCell CRs in the namespace and loads them
// into the HNSW index. Called once at startup before serving requests.
func (s *CellSyncer) SeedAll(ctx context.Context, namespace string) error {
	list := &mcell.MemoryCellList{}
	if err := s.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list MemoryCells: %w", err)
	}

	seeded, embedded, skipped := 0, 0, 0
	for i := range list.Items {
		cell := &list.Items[i]
		if cell.Spec.Status == "stale" || cell.Spec.Status == "superseded" {
			skipped++
			continue
		}
		hadEmbedding := cell.Spec.Embedding != ""
		if err := s.syncCell(ctx, cell); err != nil {
			s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("seed failed")
			skipped++
			continue
		}
		switch {
		case cell.Spec.Kind == "system_configuration":
			// operational cell — not indexed, not counted
		case isPrivateScope(cell.Spec.Scope):
			seeded++ // private cell loaded into in-memory store, no HNSW
		case hadEmbedding:
			seeded++
		default:
			embedded++
		}
	}
	s.logger.Info().
		Int("seeded_from_embedding", seeded).
		Int("newly_embedded", embedded).
		Int("skipped", skipped).
		Msg("MemoryCell seed complete")
	return nil
}

// Reconcile handles create/update/delete of MemoryCell CRs.
func (s *CellSyncer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cell := &mcell.MemoryCell{}
	if err := s.Get(ctx, req.NamespacedName, cell); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted, here or on another replica: drop the cell from the indexes.
			s.idsMu.Lock()
			id, known := s.ids[req.Name]
			delete(s.ids, req.Name)
			s.idsMu.Unlock()
			if known {
				s.memory.forget(id, true)
			}
			// De-register any in-memory map this cell carried. The cell's
			// spec is gone, so recover the label from its resource name — the same
			// name syncMapCell's label derives from (mental-map-<subject> / the id).
			if s.mapReg != nil {
				label := mapLabelFromResourceName(req.Name)
				s.mapReg.RemoveSnapshotByLabel(label)
				s.mapMu.Lock()
				delete(s.mapHashes, label)
				s.mapMu.Unlock()
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get MemoryCell: %w", err)
	}

	s.idsMu.Lock()
	s.ids[req.Name] = cell.Spec.ID
	s.idsMu.Unlock()

	if cell.Spec.Status == "stale" || cell.Spec.Status == "superseded" {
		// Tombstoned: drop the map and the indexes but keep the cell.
		s.removeMapCell(cell)
		s.memory.applyRemote(cellToItem(cell))
		return ctrl.Result{}, nil
	}

	if err := s.syncCell(ctx, cell); err != nil {
		s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("reconcile failed; will retry")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if s.mapPending(cell) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// SyncCell reconciles a single MemoryCell into the in-memory store.
func (s *CellSyncer) SyncCell(ctx context.Context, cell *mcell.MemoryCell) error {
	return s.syncCell(ctx, cell)
}

func (s *CellSyncer) syncCell(ctx context.Context, cell *mcell.MemoryCell) error {
	// Graph-carrying cell (Data["graph"]): register its gob as an in-memory map so
	// search_graph et al. can query it — no OCI image-volume, no restart. The cell's
	// text still indexes normally below (it's the map's human-facing description);
	// only the graph payload takes this extra step.
	_, hasGraph := cell.Spec.Data[graphDataKey]
	_, hasGraphRef := cell.Spec.Data[graphRefDataKey]
	if hasGraph || hasGraphRef {
		if err := s.syncMapCell(ctx, cell); err != nil {
			// Non-fatal: a bad gob shouldn't wedge the cell's normal indexing or the
			// reconcile. Log and continue so the descriptive item is still searchable.
			s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("register graph-carrying cell as map failed")
		}
	}

	// Shared cells: follow what the cluster holds (keyword index, change events).
	s.memory.applyRemote(cellToItem(cell))

	// system_configuration is never indexed and not loaded into the in-memory store.
	if cell.Spec.Kind == "system_configuration" {
		if cell.Spec.Embedding != "" {
			cell.Spec.Embedding = ""
			if err := s.Update(ctx, cell); err != nil && !apierrors.IsConflict(err) {
				s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("clear stale embedding failed")
			}
		}
		return nil
	}

	// Private cells (scope=private, e.g. personal notes): load into the in-memory
	// store (no HNSW indexing, never on the shared graph) even without a k8s cache
	// reader. (ADR-037; formerly keyed on kind==agent_note.)
	if isPrivateScope(cell.Spec.Scope) {
		if cell.Spec.Embedding != "" {
			cell.Spec.Embedding = ""
			if err := s.Update(ctx, cell); err != nil && !apierrors.IsConflict(err) {
				s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("clear stale embedding failed")
			}
		}
		// Always seed into s.items — Upsert always writes/reads s.items, so private
		// cells must be present there regardless of whether a cache reader exists.
		// Skip the write when the CR's content already matches what's in memory:
		// every reconcile of an unchanged cell (the embedding-clear Update just
		// above, informer resyncs, startup replay) would otherwise unconditionally
		// clobber whatever is currently held with a fresh read of the CR — silent
		// today (this branch never publishes an SSE event), but still a needless
		// overwrite of possibly-fresher in-memory state. ADR-033 discipline, ported
		// from the sibling triage repo's MirrorMemoryItem.
		it := cellToItem(cell)
		s.memory.mu.Lock()
		if existing, ok := s.memory.items[it.ID]; !ok || !itemsSemanticallyEqual(existing, it) {
			s.memory.items[it.ID] = it
		}
		s.memory.mu.Unlock()
		return nil
	}

	// Keyword-only mode: no HNSW index to maintain; cache is the source of truth.
	if s.memory.cells == nil {
		return s.markIndexed(ctx, cell)
	}

	doc := cellSpecToDoc(cell.Spec)

	if cell.Spec.Embedding != "" {
		vec, err := vectorstore.DecodeEmbedding(cell.Spec.Embedding)
		if err != nil {
			s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("bad embedding; will re-embed")
		} else if s.memory.cells.SeedFromEmbedding(doc, vec) {
			return s.markIndexed(ctx, cell)
		} else {
			s.logger.Warn().Str("cell", cell.Name).Int("dims", len(vec)).Msg("stored embedding has the wrong dimension; will re-embed")
		}
	}

	// No embedding — call embedder and patch vector back to CR.
	vec, err := s.memory.cells.Index(ctx, doc)
	if err != nil {
		return fmt.Errorf("embed cell %s: %w", cell.Name, err)
	}

	cell.Spec.Embedding = vectorstore.EncodeEmbedding(vec)
	cell.Status.Indexed = true
	now := metav1.Now()
	cell.Status.LastIndexedAt = &now

	if err := s.Update(ctx, cell); err != nil {
		if apierrors.IsConflict(err) {
			return nil // will reconcile again on next watch event
		}
		return fmt.Errorf("patch cell embedding: %w", err)
	}
	if err := s.Status().Update(ctx, cell); err != nil && !apierrors.IsConflict(err) {
		s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("status update failed")
	}
	return nil
}

// mapLabelFromResourceName recovers the map label from a cell's k8s resource name
// on the delete path, where the spec (and its Subject) is already gone. Mental-map
// cells are named "mental-map-<subject>" with subject as the label, so strip that
// prefix; otherwise fall back to the raw name. RemoveSnapshotByLabel is a no-op on
// a miss, so an imperfect guess is harmless.
func mapLabelFromResourceName(name string) string {
	if l := strings.TrimPrefix(name, "mental-map-"); l != name {
		return l
	}
	return name
}

// mapLabelForCell is the map label a graph-carrying cell registers under. It
// mirrors the mental-map convention (subject = map name), falling back to the
// cell ID so the label is always stable and derivable on delete without decoding
// the gob. The snapshot's own header label wins at load time; this is only the
// key used for de-registration.
func mapLabelForCell(cell *mcell.MemoryCell) string {
	if cell.Spec.Subject != "" {
		return cell.Spec.Subject
	}
	return cell.Spec.ID
}

// syncMapCell decodes the cell's graph blob and (re-)registers it as an in-memory
// map. Replacing an existing label drops the old graph first so an update swaps
// cleanly rather than leaking the prior in-memory copy. No-op when no registrar
// is wired.
func (s *CellSyncer) syncMapCell(ctx context.Context, cell *mcell.MemoryCell) error {
	if s.mapReg == nil {
		return nil
	}
	label := mapLabelForCell(cell)

	// The graph is inline (Data["graph"], base64) or a digest-pinned OCI reference
	// (Data["graphRef"], plain text); inline wins when both are present.
	var (
		payload string // what the change check hashes: the base64 blob, or the reference
		load    func() ([]byte, error)
	)
	if e, ok := cell.Spec.Data[graphDataKey]; ok && e.Bytes != "" {
		payload = e.Bytes
		load = func() ([]byte, error) {
			raw, err := base64.StdEncoding.DecodeString(e.Bytes)
			if err != nil {
				return nil, fmt.Errorf("decode graph blob: %w", err)
			}
			return raw, nil
		}
	} else if e, ok := cell.Spec.Data[graphRefDataKey]; ok && strings.TrimSpace(e.Bytes) != "" {
		payload = strings.TrimSpace(e.Bytes)
		load = func() ([]byte, error) { return s.pullGraph(ctx, cell.Namespace, payload) }
	} else {
		return nil
	}

	// Skip when this label's graph is what is already registered: re-registering
	// re-synthesizes the routing vector (an embedder call), and the startup
	// reconcile burst replays each cell several times. Hash the base64 string, or
	// the reference, directly: it changes iff the graph does, and a hit decodes or
	// pulls nothing.
	sum := sha1.Sum([]byte(payload)) //nolint:gosec
	h := hex.EncodeToString(sum[:])
	s.mapMu.Lock()
	if s.mapHashes == nil {
		s.mapHashes = map[string]string{}
	}
	unchanged := s.mapHashes[label] == h
	s.mapMu.Unlock()
	if unchanged {
		return nil
	}

	raw, err := load()
	if err != nil {
		return err
	}
	// Drop any prior registration under this label before re-adding so an update
	// replaces the graph instead of leaving the stale copy resident.
	s.mapReg.RemoveSnapshotByLabel(label)
	if err := s.mapReg.AddSnapshotBytes(ctx, raw); err != nil {
		return fmt.Errorf("register map snapshot: %w", err)
	}
	s.mapMu.Lock()
	s.mapHashes[label] = h
	s.mapMu.Unlock()
	s.logger.Info().Str("cell", cell.Name).Str("label", label).
		Int("bytes", len(raw)).Msg("registered graph-carrying cell as in-memory map")
	return nil
}

// pullGraph pulls the artifact a graphRef names.
func (s *CellSyncer) pullGraph(ctx context.Context, namespace, ref string) ([]byte, error) {
	digest, err := parseGraphRef(ref)
	if err != nil {
		return nil, err
	}
	kc, err := pullKeychain(ctx, s.Client, namespace, s.pullSecret)
	if err != nil {
		return nil, err
	}
	fetch := s.graphFetch
	if fetch == nil {
		fetch = fetchGraphArtifact
	}
	return fetch(ctx, digest, kc)
}

// mapPending reports a graphRef cell whose map has not registered yet, because
// the pull failed or the registry was unreachable. The reconciler retries it.
func (s *CellSyncer) mapPending(cell *mcell.MemoryCell) bool {
	if s.mapReg == nil {
		return false
	}
	if e, ok := cell.Spec.Data[graphDataKey]; ok && e.Bytes != "" {
		return false
	}
	e, ok := cell.Spec.Data[graphRefDataKey]
	if !ok || strings.TrimSpace(e.Bytes) == "" {
		return false
	}
	sum := sha1.Sum([]byte(strings.TrimSpace(e.Bytes))) //nolint:gosec
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	return s.mapHashes[mapLabelForCell(cell)] != hex.EncodeToString(sum[:])
}

// removeMapCell de-registers a map that was carried by a (now-deleted) cell.
// Derives the label the same way syncMapCell registered it. No-op when no
// registrar is wired.
func (s *CellSyncer) removeMapCell(cell *mcell.MemoryCell) {
	if s.mapReg == nil {
		return
	}
	label := mapLabelForCell(cell)
	s.mapReg.RemoveSnapshotByLabel(label)
	// Forget the hash so a later recreate re-registers rather than being skipped
	// as "unchanged".
	s.mapMu.Lock()
	delete(s.mapHashes, label)
	s.mapMu.Unlock()
}

func (s *CellSyncer) markIndexed(ctx context.Context, cell *mcell.MemoryCell) error {
	if cell.Status.Indexed {
		return nil
	}
	cell.Status.Indexed = true
	now := metav1.Now()
	cell.Status.LastIndexedAt = &now
	if err := s.Status().Update(ctx, cell); err != nil && !apierrors.IsConflict(err) {
		s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("status update failed")
	}
	return nil
}

// Persist writes a MemoryItem to the cluster as an MemoryCell CR.
// Preserves any existing embedding so the reconciler won't re-embed unnecessarily.
func (s *CellSyncer) Persist(ctx context.Context, it *MemoryItem, namespace string) error {
	name := cellResourceName(it.ID)
	now := metav1.Now()

	existing := &mcell.MemoryCell{}
	err := s.Get(ctx, k8stypes.NamespacedName{Namespace: namespace, Name: name}, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get cell %s: %w", name, err)
	}

	if apierrors.IsNotFound(err) {
		cell := &mcell.MemoryCell{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{"xray.triageagent.dev/managed": "true", "xray.triageagent.dev/kind": it.Kind},
			},
			Spec: mcell.MemoryCellSpec{
				ID:         it.ID,
				Kind:       it.Kind,
				Scope:      it.Scope,
				Subject:    it.Subject,
				Title:      it.Title,
				Content:    it.Content,
				Confidence: it.Confidence,
				EventDate:  it.EventDate,
				Tags:       it.Tags,
				Data:       it.Data,
				Status:     cellStatus(it.Status),
				StaleAfter: timePtrToMetaTime(it.StaleAfter),
				CreatedAt:  now,
				UpdatedAt:  now,

				Origin:       it.Origin,
				EvidenceRefs: it.EvidenceRefs,
				Supersedes:   it.Supersedes,
				Links:        it.Links,
			},
		}
		if err := s.Create(ctx, cell); err != nil {
			return err
		}
		cell.Status.Indexed = true
		cell.Status.LastIndexedAt = &now
		if err := s.Status().Update(ctx, cell); err != nil && !apierrors.IsConflict(err) {
			s.logger.Warn().Err(err).Str("cell", cell.Name).Msg("status update failed")
		}
		return nil
	}

	// Desired content, everything Persist would otherwise overwrite except the
	// preserved embedding and the CreatedAt/UpdatedAt timestamps (filled in below
	// only once we know a write is actually happening). Compare against what's
	// already stored first: an identical re-Persist (a re-upserting agent loop,
	// a redundant voice/MCP write) must not touch the CR at all — no Update, no
	// UpdatedAt stamp, no status update — or every idempotent write still bumps
	// resourceVersion and re-fires the CellSyncer's own reconcile (ADR-033
	// discipline, ported from the sibling triage repo).
	//
	// StaleAfter has a MemoryItem counterpart (see MemoryItem.StaleAfter), so
	// it is WRITTEN here like every other item-carried field — Persist is the
	// caller's authoritative statement of the item's current deadline.
	// VoiceConfig/MCP/UI have NO MemoryItem counterpart (kubectl and the
	// system_configuration tooling write them): Persist only ever mutates fields
	// the in-memory item actually carries, so those are copied forward from the
	// existing CR instead of zeroing them. Origin, EvidenceRefs, Supersedes and
	// Links are carried by the item when set and copied forward when not.
	desired := mcell.MemoryCellSpec{
		ID:         it.ID,
		Kind:       it.Kind,
		Scope:      it.Scope,
		Subject:    it.Subject,
		Title:      it.Title,
		Content:    it.Content,
		Confidence: it.Confidence,
		EventDate:  it.EventDate,
		Tags:       it.Tags,
		Data:       it.Data,
		Status:     cellStatus(it.Status),
		StaleAfter: timePtrToMetaTime(it.StaleAfter),

		// Origin, EvidenceRefs, Supersedes and Links are the item's when it
		// carries them (a write, a refute, a consolidation) and the stored
		// CR's otherwise.
		Origin:       firstNonEmpty(it.Origin, existing.Spec.Origin),
		EvidenceRefs: firstNonNil(it.EvidenceRefs, existing.Spec.EvidenceRefs),
		Supersedes:   firstNonNil(it.Supersedes, existing.Spec.Supersedes),
		Links:        firstNonNilLinks(it.Links, existing.Spec.Links),
		VoiceConfig:  existing.Spec.VoiceConfig,
		MCP:          existing.Spec.MCP,
		UI:           existing.Spec.UI,
	}
	if specContentEqual(desired, existing.Spec) {
		return nil
	}

	existingEmbedding := existing.Spec.Embedding
	desired.CreatedAt = existing.Spec.CreatedAt
	desired.UpdatedAt = now
	desired.Embedding = existingEmbedding
	existing.Spec = desired
	if err := s.Update(ctx, existing); err != nil {
		return err
	}
	existing.Status.Indexed = true
	existing.Status.LastIndexedAt = &now
	if err := s.Status().Update(ctx, existing); err != nil && !apierrors.IsConflict(err) {
		s.logger.Warn().Err(err).Str("cell", existing.Name).Msg("status update failed")
	}
	return nil
}

// specContentEqual reports whether two MemoryCell specs carry the same
// semantic content — every field Persist's update path writes, EXCLUDING
// Embedding (preserved separately, never compared here) and the
// CreatedAt/UpdatedAt timestamps (exactly what we are trying not to churn).
// Tags/Data compare nil and empty as equal (slices.Equal/maps.Equal); so do
// EvidenceRefs/Supersedes (slices.Equal) and Links (slices.EqualFunc, field by
// field). VoiceConfig/MCP/UI are pointers to composite structs with nested
// slices/pointers of their own (voice session params, MCP server lists, UI
// category taxonomy) — reflect.DeepEqual compares them by value (nil-safe,
// treats two independently-built-but-identical structs as equal) rather than
// hand-rolling a field-by-field comparator for each.
func specContentEqual(a, b mcell.MemoryCellSpec) bool {
	return a.ID == b.ID &&
		a.Kind == b.Kind &&
		a.Scope == b.Scope &&
		a.Subject == b.Subject &&
		a.Title == b.Title &&
		a.Content == b.Content &&
		a.Confidence == b.Confidence &&
		a.EventDate == b.EventDate &&
		a.Status == b.Status &&
		a.Origin == b.Origin &&
		slices.Equal(a.Tags, b.Tags) &&
		maps.Equal(a.Data, b.Data) &&
		metaTimeEqual(a.StaleAfter, b.StaleAfter) &&
		slices.Equal(a.EvidenceRefs, b.EvidenceRefs) &&
		slices.Equal(a.Supersedes, b.Supersedes) &&
		slices.EqualFunc(a.Links, b.Links, thoughtLinkEqual) &&
		reflect.DeepEqual(a.VoiceConfig, b.VoiceConfig) &&
		reflect.DeepEqual(a.MCP, b.MCP) &&
		reflect.DeepEqual(a.UI, b.UI)
}

// cellStatus is the CR status of an in-process item: stale and superseded cells
// are kept, everything else (active, or indexed in this pod) is stored active.
func cellStatus(status string) string {
	if status == "stale" || status == "superseded" {
		return status
	}
	return "active"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonNil(a, b []string) []string {
	if a != nil {
		return a
	}
	return b
}

func firstNonNilLinks(a, b []mcell.ThoughtLink) []mcell.ThoughtLink {
	if a != nil {
		return a
	}
	return b
}

// metaTimeEqual reports whether two optional CR timestamps are equal: both
// nil, or both non-nil and instant-equal (time.Time.Equal, not ==).
func metaTimeEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Time.Equal(b.Time)
}

// thoughtLinkEqual compares two ThoughtLink entries field by field, treating
// nil and empty EvidenceRefs as equal like every other slice field here.
func thoughtLinkEqual(a, b mcell.ThoughtLink) bool {
	return a.ToID == b.ToID &&
		a.Relation == b.Relation &&
		a.Weight == b.Weight &&
		slices.Equal(a.EvidenceRefs, b.EvidenceRefs)
}

// Remove deletes an MemoryCell CR by item ID.
func (s *CellSyncer) Remove(ctx context.Context, id, namespace string) error {
	cell := &mcell.MemoryCell{}
	err := s.Get(ctx, k8stypes.NamespacedName{Namespace: namespace, Name: cellResourceName(id)}, cell)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get cell: %w", err)
	}
	// Another replica may delete it between the Get and here; that is the goal state.
	if err := s.Delete(ctx, cell); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// cellResourceName converts a memory item ID to a DNS-1123-compliant CR name.
func cellResourceName(id string) string {
	s := strings.ToLower(id)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "cell"
	}
	const maxBase = 244
	if len(base) > maxBase {
		base = strings.TrimRight(base[:maxBase], "-")
	}
	sum := sha1.Sum([]byte(id)) //nolint:gosec
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// SetupWithManager registers the CellSyncer as a controller.
func (s *CellSyncer) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcell.MemoryCell{}).
		Complete(s)
}

func cellSpecToDoc(spec mcell.MemoryCellSpec) vectorstore.MemoryCellIndexEntry {
	return vectorstore.MemoryCellIndexEntry{
		ID:         spec.ID,
		Kind:       spec.Kind,
		Scope:      spec.Scope,
		Subject:    spec.Subject,
		Title:      spec.Title,
		Content:    spec.Content,
		Confidence: spec.Confidence,
		Status:     spec.Status,
		Tags:       spec.Tags,
		EventDate:  spec.EventDate,
	}
}
