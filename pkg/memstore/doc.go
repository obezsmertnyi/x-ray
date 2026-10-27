// Package memstore is the memory engine behind X-Ray, for a Go program that
// wants the same store, search and ranking in its own process.
//
// An Item is a MemoryCell spec plus a few in-process fields. A host brings its
// own rules through small seams (a visibility predicate, a retention function,
// ranking callbacks, a kind vocabulary) and its own lock; the package reads
// no global state and knows no Kubernetes client. The cell index and the
// embedder are in the subpackage index, the change hub in hub.
package memstore
