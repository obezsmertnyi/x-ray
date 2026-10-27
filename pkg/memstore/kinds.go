package memstore

// Kinds the engine itself knows by name. Every other kind belongs to the host,
// which declares it through Vocabulary.
const (
	// KindStableFact is established truth about a subject, e.g. a verified root
	// cause. It needs high confidence.
	KindStableFact = "stable_fact"
	// KindOperationalPattern is a recurring behaviour worth remembering.
	KindOperationalPattern = "operational_pattern"
	// KindWatchItem is something being watched; it ranks higher while active.
	KindWatchItem = "watch_item"
	// KindHypothesis is a claim not yet confirmed; it decays quickly.
	KindHypothesis = "hypothesis"
	// KindUnknown is an open question; it decays.
	KindUnknown = "unknown"
	// KindEvent is something that happened at a time of its own (EventDate).
	KindEvent = "event"
)
