package engine

import (
	"sync"
	"sync/atomic"
)

// RegistryStats accumulates process-lifetime routing and map-lifecycle
// metrics for a Registry (Mode B): which map(s) actually got
// searched and how often, whether a search was explicitly addressed to a map
// or semantically routed, how wide a semantic fanout actually consulted, and
// how often a map's body was lazily loaded from its snapshot or evicted back
// out. It is the request/registry-level complement to index.SearchStats
// (query-mechanics-level: the semantic/lexical partition and latency inside
// one map's search) — Registry injects one shared *index.SearchStats into
// every *index.Index it builds (see load) precisely so those counts also
// survive the evict/reload cycle that recreates each project's Index; this
// type is the Registry's own equivalent, owned directly since Registry
// itself is the one object that already lives for the whole process.
//
// Every counter here is either a lock-free atomic or a small map guarded by
// an RWMutex whose VALUES are themselves atomics — so incrementing an
// existing map's counters never blocks a concurrent increment of a
// different map's counters, only the rare "first search for this label"
// path takes the write lock.
type RegistryStats struct {
	explicitRoutes  atomic.Int64
	semanticRoutes  atomic.Int64
	fanoutConsulted atomic.Int64 // sum of maps consulted, semantic routes only
	fanoutRoutes    atomic.Int64 // denominator for the average above

	loads     atomic.Int64
	evictions atomic.Int64

	mapMu sync.RWMutex
	maps  map[string]*mapCounters
}

// mapCounters is one map's search/hit totals.
type mapCounters struct {
	searches atomic.Int64
	hits     atomic.Int64
}

// NewRegistryStats returns a fresh, zeroed RegistryStats.
func NewRegistryStats() *RegistryStats {
	return &RegistryStats{maps: make(map[string]*mapCounters)}
}

// recordExplicitRoute marks a Resolve call that matched an exact registered
// label — the free fast path that needs no embedding call.
func (s *RegistryStats) recordExplicitRoute() { s.explicitRoutes.Add(1) }

// recordSemanticRoute marks a Resolve call that went through embedding-based
// routing (no exact label match, including an inexact map hint), and how
// many maps the resulting fanout actually consulted.
func (s *RegistryStats) recordSemanticRoute(consulted int) {
	s.semanticRoutes.Add(1)
	s.fanoutConsulted.Add(int64(consulted))
	s.fanoutRoutes.Add(1)
}

// recordLoad marks one map body actually loaded (or reloaded) from its
// snapshot — as opposed to a Resolve/RecordSearch hitting an already-resident
// map. A search that triggers a cold load has a very different latency
// profile than one that doesn't; this is what makes the two distinguishable.
func (s *RegistryStats) recordLoad() { s.loads.Add(1) }

// recordEviction marks one map's in-memory body dropped after its idle TTL.
func (s *RegistryStats) recordEviction() { s.evictions.Add(1) }

// RecordSearch attributes one map's search + returned-hit count. Exported
// because the caller that knows both a search's resolved map label and its
// hit count is the mcpserver package's search loop (over Resolve's result),
// not Registry itself — Registry only decides WHICH maps to consult, not how
// many hits each one returns. mcpserver.Resolver reaches it as a narrow
// interface method so mcpserver need not import this package's concrete
// type.
func (s *RegistryStats) RecordSearch(label string, hits int) {
	s.mapMu.RLock()
	c, ok := s.maps[label]
	s.mapMu.RUnlock()
	if !ok {
		s.mapMu.Lock()
		c, ok = s.maps[label]
		if !ok {
			c = &mapCounters{}
			s.maps[label] = c
		}
		s.mapMu.Unlock()
	}
	c.searches.Add(1)
	c.hits.Add(int64(hits))
}

// RoutingSnapshot is a point-in-time readout of routing outcomes.
type RoutingSnapshot struct {
	ExplicitRoutes     int64   `json:"explicitRoutes"`
	SemanticRoutes     int64   `json:"semanticRoutes"`
	AvgFanoutConsulted float64 `json:"avgFanoutConsulted"`
}

// MapCounters is one map's search/hit totals, JSON-friendly.
type MapCounters struct {
	Searches int64 `json:"searches"`
	Hits     int64 `json:"hits"`
}

// RegistryStatsSnapshot is a point-in-time, JSON-friendly readout of
// RegistryStats. (Named to avoid colliding with the unrelated graph Snapshot
// type this package already exports — see snapshot.go.)
type RegistryStatsSnapshot struct {
	Routing      RoutingSnapshot        `json:"routing"`
	Maps         map[string]MapCounters `json:"maps"`
	MapLoads     int64                  `json:"mapLoads"`
	MapEvictions int64                  `json:"mapEvictions"`
}

// Snapshot reads every counter at one point in time.
func (s *RegistryStats) Snapshot() RegistryStatsSnapshot {
	fc, fr := s.fanoutConsulted.Load(), s.fanoutRoutes.Load()
	var avg float64
	if fr > 0 {
		avg = float64(fc) / float64(fr)
	}

	s.mapMu.RLock()
	maps := make(map[string]MapCounters, len(s.maps))
	for label, c := range s.maps {
		maps[label] = MapCounters{Searches: c.searches.Load(), Hits: c.hits.Load()}
	}
	s.mapMu.RUnlock()

	return RegistryStatsSnapshot{
		Routing: RoutingSnapshot{
			ExplicitRoutes:     s.explicitRoutes.Load(),
			SemanticRoutes:     s.semanticRoutes.Load(),
			AvgFanoutConsulted: avg,
		},
		Maps:         maps,
		MapLoads:     s.loads.Load(),
		MapEvictions: s.evictions.Load(),
	}
}
