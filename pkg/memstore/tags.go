package memstore

import "strings"

// ServiceTagPrefix tags an item with the service or entity it concerns, e.g.
// "service/auth". It is the tag-side home of that association, so Subject can
// hold the instance identity.
const ServiceTagPrefix = "service/"

// ServiceTagValue returns the value of an item's service/<name> tag, if any.
// Lookups that filter by subject check this in addition to Subject, so they
// match both items whose Subject holds the service name directly and items that
// carry it as a tag.
func ServiceTagValue(item Item) (string, bool) {
	for _, t := range item.Tags {
		if v, ok := strings.CutPrefix(t, ServiceTagPrefix); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// AuthorTagPrefix is a reserved tag ("author/<identity>") a write path stamps to
// record the authenticated caller that last authored a cell, so a reader can
// treat the cell as that identity's claim rather than trusted derived memory.
// Machine writers never stamp it; StripAuthorTags removes it when a machine
// rewrites the cell.
const AuthorTagPrefix = "author/"

// AuthorTagValue returns the identity from an item's first author/<identity>
// tag, if any.
func AuthorTagValue(item Item) (string, bool) {
	for _, t := range item.Tags {
		if v, ok := strings.CutPrefix(t, AuthorTagPrefix); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// StampAuthorTag leaves item.Tags with a single author/<author> tag. It first
// removes every existing author/ tag, so a caller-supplied tag list cannot
// assert a different author than the one the write path resolved, then appends
// the new one. It is a no-op when author is "".
//
// It is idempotent: stamping the same author twice yields the same Tags, which
// keeps the cell to memory round trip free of spurious writes.
func StampAuthorTag(item *Item, author string) {
	if author == "" {
		return
	}
	item.Tags = StripAuthorTags(item.Tags)
	item.Tags = append(item.Tags, AuthorTagPrefix+author)
}

// StripAuthorTags returns tags without any author/ entry, keeping the order of
// the rest. It is nil-safe.
func StripAuthorTags(tags []string) []string {
	if tags == nil {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if strings.HasPrefix(t, AuthorTagPrefix) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// TopicTagPrefix tags an item with a free-form value a model supplied for
// Subject that was not accepted as a real subject, e.g. "topic/memory": the
// generic counterpart of ServiceTagPrefix.
const TopicTagPrefix = "topic/"

// SanitizeTagValue makes v safe to use as the value segment of a tag: it trims
// whitespace and turns commas (the separator tag lists are split on) and
// newlines into a single space, so a supplied value cannot be re-split into
// several tags on a later round trip or break a one-line render.
func SanitizeTagValue(v string) string {
	v = strings.TrimSpace(v)
	replacer := strings.NewReplacer(",", " ", "\n", " ", "\r", " ")
	v = replacer.Replace(v)
	for strings.Contains(v, "  ") {
		v = strings.ReplaceAll(v, "  ", " ")
	}
	return strings.TrimSpace(v)
}

// PartitionTagPrefix is the tag prefix that carries an item's partition, e.g.
// "tenant/alpha": the unit a host isolates memory by. The "/" separator keeps
// it apart from tags that use ":". The spelling is part of the stored cells.
const PartitionTagPrefix = "tenant/"

// PartitionFromTags returns the partition named by the tags, or "" when none is.
func PartitionFromTags(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, PartitionTagPrefix) {
			return strings.TrimPrefix(t, PartitionTagPrefix)
		}
	}
	return ""
}

// SharedTag is the literal tag that marks a cell as deliberately visible to
// every partition. An untagged cell is private to its own partition; this mark
// is the one way a host lets it out. It combines with a partition tag: that
// means "belongs to this partition and is also shared with the others". Only a
// trusted writer should be able to set it; a write path strips it from other
// callers' tags first, as it does author/ and partition tags.
const SharedTag = "shared"

// HasSharedTag reports whether tags carries SharedTag.
func HasSharedTag(tags []string) bool {
	for _, t := range tags {
		if t == SharedTag {
			return true
		}
	}
	return false
}

// IsSharedItem reports whether item carries SharedTag.
func IsSharedItem(item Item) bool {
	return HasSharedTag(item.Tags)
}
