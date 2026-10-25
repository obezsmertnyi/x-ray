package index

// Persisting the built HNSW graph into a snapshot so a cold load can DESERIALIZE
// it instead of rebuilding it from scratch. The rebuild (hnsw.Insert over every
// vector, O(N·logN·M)) is the dominant cost of loading a snapshot — for a ~4.5k
// node graph it is several seconds, and it recurs on every pod start and after
// idle eviction. fogfish/hnsw exposes Write(Writer)/Read(Reader) over a simple
// key/value codec; we back that with an in-memory map so the whole ANN graph
// rides along in the snapshot body and reload skips the insert loop entirely.

import (
	"github.com/fogfish/hnsw"
)

// kvMap is the in-memory key/value store the hnsw codec writes to / reads from.
// Keys are the codec's internal node keys ("&root", "&<id>"); values are the
// binary-marshalled header/nodes. It satisfies both hnsw.Writer and hnsw.Reader.
type kvMap map[string][]byte

func (m kvMap) Put(k, v []byte) error { m[string(k)] = append([]byte(nil), v...); return nil }

func (m kvMap) Get(k []byte) ([]byte, error) { return m[string(k)], nil }

// DumpHNSW serializes the current ANN index to a flat map suitable for gob
// encoding into a snapshot body. Returns nil if no ANN index is built yet.
func (ix *Index) DumpHNSW() map[string][]byte {
	ix.mu.Lock()
	ann := ix.ann
	ix.mu.Unlock()
	if ann == nil {
		return nil
	}
	kv := kvMap{}
	if err := ann.Write(kv); err != nil {
		ix.logger.Warn().Err(err).Msg("hnsw dump failed; snapshot will rebuild the index on load")
		return nil
	}
	return kv
}

// LoadHNSW reconstructs the ANN index from a previously dumped map, skipping the
// O(N·logN) rebuild. Returns true on success; on any error the caller should
// fall back to rebuildANNLocked. The HNSW is created with the SAME construction
// params as rebuildANNLocked so Read overwrites a compatible shell.
func (ix *Index) LoadHNSW(dump map[string][]byte) bool {
	if len(dump) == 0 {
		return false
	}
	g := hnsw.New(annSurface,
		hnsw.WithEfConstruction(200),
		hnsw.WithM(16),
		hnsw.WithM0(32),
	)
	if err := g.Read(kvMap(dump)); err != nil {
		ix.logger.Warn().Err(err).Msg("hnsw load failed; rebuilding index from vectors")
		return false
	}
	ix.mu.Lock()
	ix.ann = g
	ix.mu.Unlock()
	ix.logger.Debug().Int("vectors", g.Size()).Msg("loaded HNSW index from snapshot (no rebuild)")
	return true
}

// tryLoadHNSW loads a serialized ANN graph and validates it covers essentially
// all of the live graph's embedded nodes. An exact match is NOT required: the
// HNSW indexes only nodes that had a vector when it was built, which can be a
// few fewer than the reload's all-cached count (e.g. nodes whose vector wasn't
// set at ANN-build time). A SMALL shortfall is fine — those nodes have no vector
// to search anyway. A LARGE shortfall means the dump is stale relative to its
// node set, so rebuild is safer. Threshold: the HNSW must cover ≥99% of embedded
// nodes (and never be larger, which would indicate a corrupt/foreign dump).
func (ix *Index) tryLoadHNSW(dump map[string][]byte) bool {
	if len(dump) == 0 {
		return false
	}
	embedded := 0
	for _, nd := range ix.g.Nodes() {
		if nd.Vec != nil {
			embedded++
		}
	}
	if !ix.LoadHNSW(dump) {
		return false
	}
	ix.mu.Lock()
	size := 0
	if ix.ann != nil {
		size = ix.ann.Size()
	}
	ix.mu.Unlock()
	// Reject a dump that is larger than the node set (corrupt/foreign) or that
	// covers materially fewer than the embedded nodes (stale). The ~1% benign
	// shortfall is nodes that had no usable vector at ANN-build time (empty-text/
	// File nodes) — unsearchable anyway, so a 95% floor is safe.
	if size > embedded || (embedded > 0 && size < embedded*95/100) {
		ix.logger.Warn().Int("hnswSize", size).Int("embedded", embedded).
			Msg("snapshot HNSW coverage too far from embedded nodes; rebuilding to stay correct")
		return false
	}
	return true
}
