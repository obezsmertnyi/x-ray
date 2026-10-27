package memstore

import "strings"

// ScopePrivate marks an item as isolated to its owner: visible to that one
// caller and nowhere else, not in shared search, not on the change stream. Any
// other Scope value is shared.
const ScopePrivate = "private"

// ScopePublic marks an item as shared: visible on the graph and in search.
const ScopePublic = "public"

// PrivateOwnerTagPrefix tags a private item with its owner's identity hash.
// Subject names the instance that holds the item, not a person, so it cannot
// carry ownership: the tag is the only place a private item's owner lives.
const PrivateOwnerTagPrefix = "user:"

// IsPrivateItem reports whether it is scoped private.
func IsPrivateItem(it Item) bool {
	return it.Scope == ScopePrivate
}

// PrivateOwner returns the owner hash carried in the item's user: tag, or ""
// when there is none.
func PrivateOwner(it Item) string {
	for _, t := range it.Tags {
		if owner, ok := strings.CutPrefix(t, PrivateOwnerTagPrefix); ok {
			return owner
		}
	}
	return ""
}

// VisibleToUser reports whether it is visible to a caller identified by
// userHash. A shared item is visible to everyone. A private item is visible
// only to the exact hash tagged as its owner: no administrator, partition or
// operator bypass.
func VisibleToUser(it Item, userHash string) bool {
	if !IsPrivateItem(it) {
		return true
	}
	return userHash != "" && PrivateOwner(it) == userHash
}

// FilterVisibleToUser narrows items to those VisibleToUser allows for userHash.
func FilterVisibleToUser(items []Item, userHash string) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if VisibleToUser(it, userHash) {
			out = append(out, it)
		}
	}
	return out
}
