package memstore

// Origin values stored in MemoryCellSpec.Origin.
const (
	// OriginObserved: written by code from facts it measured.
	OriginObserved = "observed"
	// OriginDerived: written by a model. A prior claim, never evidence. A cell
	// with no origin is derived.
	OriginDerived = "derived"
	// OriginHuman: written by an identified person. Outranks derived.
	OriginHuman = "human"
	// OriginVerified: derived, then passed a check.
	OriginVerified = "verified"
)

// errDerivedReplacesHuman is the validation message for a derived write onto a
// human-authored cell.
const errDerivedReplacesHuman = "a derived write cannot replace a human-authored cell"

func validOrigin(o string) bool {
	switch o {
	case OriginObserved, OriginDerived, OriginHuman, OriginVerified:
		return true
	}
	return false
}

// effectiveOrigin reads a stored origin: a cell written before the field
// existed, or without one, is derived.
func effectiveOrigin(o string) string {
	if o == "" {
		return OriginDerived
	}
	return o
}

// resolveOrigin picks the origin a write lands with. incoming is the item's own
// origin; existing is the origin of the cell it replaces (hasExisting false when
// there is none). An empty incoming origin keeps the existing one, or becomes
// derived when there is none. A derived write onto a human-origin cell is
// refused (msg non-empty).
func resolveOrigin(existing string, hasExisting bool, incoming string) (origin, msg string) {
	origin = incoming
	if origin == "" {
		if hasExisting && existing != "" {
			origin = existing
		} else {
			origin = OriginDerived
		}
	}
	if hasExisting && existing == OriginHuman && origin == OriginDerived {
		return "", errDerivedReplacesHuman
	}
	return origin, ""
}

// RestoredOrigin is the origin a cell read back from a MemoryCell carries: the
// one it has, else human when it bears an author tag, else derived.
func RestoredOrigin(item Item) string {
	if item.Origin != "" {
		return item.Origin
	}
	if _, ok := AuthorTagValue(item); ok {
		return OriginHuman
	}
	return OriginDerived
}

// originRank orders origins by how far to trust them: a person's word, then a
// checked claim, then a measured fact, then a model's guess. A cell with no
// origin is derived.
func originRank(o string) int {
	switch effectiveOrigin(o) {
	case OriginHuman:
		return 3
	case OriginVerified:
		return 2
	case OriginObserved:
		return 1
	}
	return 0
}
