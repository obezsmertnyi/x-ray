// Package memtest holds fixtures the tests of memstore and of its host share:
// the near-duplicate groups that tune the dedup threshold.
package memtest

import (
	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
)

func DedupItem(id, kind, partition, service, status, title, content string) memstore.Item {
	var tags []string
	if partition != "" {
		tags = append(tags, memstore.PartitionTagPrefix+partition)
	}
	if service != "" {
		tags = append(tags, memstore.ServiceTagPrefix+service)
	}
	return memstore.Item{
		MemoryCellSpec: memoryv1.MemoryCellSpec{
			ID:      id,
			Kind:    kind,
			Title:   title,
			Content: content,
			Status:  status,
			Tags:    tags,
		},
	}
}

// Group A: one operator-verified fact ("vector dimension mismatch is the
// search-api root cause") restated five times.
func GroupANearDup() []memstore.Item {
	return []memstore.Item{
		DedupItem("a1", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Vector store dimension mismatch confirmed as search-api root cause",
			"The search-api issue is rooted in a vector collection 'docs' having 768 dimensions while the embedding model requires 3072. This mismatch causes startup migration failures and continuous workload cycling, and has been operator-verified."),
		DedupItem("a2", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Root cause of search-api instability: vector store dimension mismatch",
			"The instability in search-api is rooted in a vector store dimension mismatch where the 'docs' collection is 768-dim but the embedding model requires 3072-dim. This mismatch causes startup migration failures and workload cycling."),
		DedupItem("a3", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Operator-verified root cause: vector dimension mismatch for search-api startup",
			"The root cause for search-api's continuous startup failures has been operator-verified as a vector collection 'docs' dimension mismatch (768-dim vs 3072-dim required by the embedding model)."),
		DedupItem("a4", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Vector dimension mismatch causes search-api workloads to cycle InProgress",
			"An operator-verified root cause for the continuous flap on search-api is a vector collection 'docs' dimension mismatch (768-dim vs 3072 for the embedding model). This prevents startup migrations and causes workloads to cycle InProgress on every restart."),
		DedupItem("a5", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Specific vector dimension mismatch for search-api confirmed",
			"The search-api application's degradation is caused by a vector collection 'docs' having 768 dimensions while the embedding model requires 3072 dimensions, verified by operator. This incompatibility leads to workloads cycling InProgress."),
	}
}

// Group B: one fact ("graph database ruled out as search-api root cause")
// restated four times.
func GroupBNearDup() []memstore.Item {
	return []memstore.Item{
		DedupItem("b1", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Graph database ruled out as primary cause for current search-api flapping",
			"The hypothesis that graph database issues were contributing to search-api degradation has been disproven, as it recovered but the flapping continued."),
		DedupItem("b2", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Graph database ruled out as root cause of search-api flapping",
			"The hypothesis that graph database issues were the root cause of search-api flapping has been disproven: graph database recovered, but the search-api flapping continued."),
		DedupItem("b3", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Hypothesis that graph database caused search-api flapping disproven",
			"Graph database was suspected as the primary cause of search-api flapping and degradation, but this hypothesis has been ruled out — graph database recovered while the search-api flapping continued."),
		DedupItem("b4", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Graph database disproven as cause of ongoing search-api flapping",
			"The hypothesis that graph database degradation was the root cause of search-api flapping has been ruled out: graph database recovered fully, yet search-api flapping continued unchanged."),
	}
}
