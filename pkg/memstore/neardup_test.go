package memstore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore/memtest"
)

// ── Fixture ───────────────────────────────────────────────────────────────
//
// All names below are invented for this test: no real partition, cluster, or
// service name appears anywhere in this file. "search-api" and "partition-a"/
// "partition-b" are placeholders only.

// Group C: one operational pattern ("AppStatusChanged flapping is expected
// noise for the known dimension-mismatch concern") restated six times.
func groupCNearDup() []memstore.Item {
	return []memstore.Item{
		memtest.DedupItem("c1", "operational_pattern", "partition-a", "search-api", "active",
			"Recurring AppStatusChanged flaps for search-api are expected noise",
			"search-api workloads cycle InProgress between 11 and 10 of 12 replicas as a byproduct of the known vector dimension mismatch issue. This AppStatusChanged flapping is expected noise and should not open new issues."),
		memtest.DedupItem("c2", "operational_pattern", "partition-a", "search-api", "active",
			"AppStatusChanged flapping on search-api is known noise, not a new issue",
			"The vector dimension mismatch issue causes search-api workloads to cycle InProgress, toggling between 11 and 10 of 12 replicas. The resulting AppStatusChanged flaps are expected and should not trigger a new issue."),
		memtest.DedupItem("c3", "operational_pattern", "partition-a", "search-api", "active",
			"search-api AppStatusChanged flaps: expected noise from vector dimension mismatch",
			"Because of the ongoing vector dimension mismatch issue, search-api workloads repeatedly cycle InProgress (11 of 12, then 10 of 12 replicas). These recurring AppStatusChanged flaps are expected noise and must not open a new issue."),
		memtest.DedupItem("c4", "operational_pattern", "partition-a", "search-api", "active",
			"Expected AppStatusChanged noise on search-api from the vector dimension mismatch issue",
			"search-api replicas cycle InProgress, flapping between 11 and 10 of 12, as expected noise from the vector dimension mismatch issue. Recurring AppStatusChanged events for this should not open new issues."),
		memtest.DedupItem("c5", "operational_pattern", "partition-a", "search-api", "active",
			"Do not open a new issue for search-api AppStatusChanged flaps",
			"The recurring AppStatusChanged flaps on search-api, with workloads cycling InProgress between 11 and 10 of 12 replicas, are expected noise caused by the vector dimension mismatch issue and should not open new issues."),
		memtest.DedupItem("c6", "operational_pattern", "partition-a", "search-api", "active",
			"search-api workload flapping 11/10 of 12 is known noise, not a new issue",
			"search-api workloads cycling InProgress 11 to 10 of 12 replicas is recurring AppStatusChanged flapping caused by the vector dimension mismatch issue. This is expected noise; do not open new issues."),
	}
}

// Distinct set: must not collapse with any group above, or with each other,
// except where noted.
func distinctNearDup() map[string]memstore.Item {
	return map[string]memstore.Item{
		// Comparable with group C (same kind/partition/service) but a different
		// pattern entirely.
		"d1_postgres_symptom": memtest.DedupItem("d1", "operational_pattern", "partition-a", "search-api", "active",
			"Postgres 'Advisory lock session lost' errors as symptom of vector dimension mismatch",
			"search-api pods intermittently log Postgres 'Advisory lock session lost' errors. This is a downstream symptom of the vector dimension mismatch issue, not a separate database problem, and does not need its own issue."),
		// Different service/ tag entirely (search-api-ingest, not
		// search-api) so it is not even comparable to the groups above.
		"d2_oom_ingest": memtest.DedupItem("d2", "operational_pattern", "partition-a", "search-api-ingest", "active",
			"Recurring OOM kills on search-api-ingest pods",
			"The search-api-ingest pods are repeatedly OOMKilled under load, unrelated to the vector dimension mismatch or the graph database investigation. Memory limits are set too low for the current batch size and need to be raised."),
		// The contradiction of group B ("graph database WAS the cause after
		// all"). Comparable with group B (same kind/partition/service); may
		// legitimately score as a near-duplicate of B under
		// newest-statement-wins semantics — asserted, not required, below.
		"d3_graph_db_contradiction": memtest.DedupItem("d3", memstore.KindStableFact, "partition-a", "search-api", "active",
			"Graph database degradation directly caused by connection errors",
			"Contrary to the earlier ruling-out, the graph database's degradation on search-api is directly caused by a spike in connection errors from the pool exhausting under load. This is now confirmed as a contributing root cause, not a red herring."),
		// No service/ tag at all — comparable with group C (one side
		// untagged still compares) but describes an unrelated, partition-wide
		// pattern.
		"d4_untagged_selfrecover": memtest.DedupItem("d4", "operational_pattern", "partition-a", "", "active",
			"Applications self-recover from AppStatusChanged during in-progress deployments",
			"Various applications across the partition briefly emit AppStatusChanged events while a deployment is in progress and self-recover once the rollout completes. This is unrelated to any specific service and is not evidence of the vector dimension mismatch issue."),
	}
}

// ── Score matrix (diagnostic) ────────────────────────────────────────────

func TestNearDuplicateMemory_ScoreMatrix(t *testing.T) {
	all := append(append(append([]memstore.Item{}, memtest.GroupANearDup()...), memtest.GroupBNearDup()...), groupCNearDup()...)
	d := distinctNearDup()
	for _, key := range []string{"d1_postgres_symptom", "d2_oom_ingest", "d3_graph_db_contradiction", "d4_untagged_selfrecover"} {
		all = append(all, d[key])
	}

	var sb strings.Builder
	sb.WriteString("pairwise score matrix (score, dup):\n")
	header := "        "
	for _, item := range all {
		header += fmt.Sprintf("%-8s", item.ID)
	}
	sb.WriteString(header + "\n")
	for _, x := range all {
		row := fmt.Sprintf("%-8s", x.ID)
		for _, y := range all {
			score, dup := memstore.NearDuplicateMemory(x, y)
			mark := "."
			if dup {
				mark = "D"
			}
			row += fmt.Sprintf("%.2f%-3s", score, mark)
		}
		sb.WriteString(row + "\n")
	}
	t.Logf("\n%s", sb.String())
}

// ── Group-internal: every pair within a group must be a duplicate ────────

func assertAllDup(t *testing.T, label string, items []memstore.Item) {
	t.Helper()
	for i := range items {
		for j := range items {
			if i == j {
				continue
			}
			score, dup := memstore.NearDuplicateMemory(items[i], items[j])
			assert.Truef(t, dup, "%s: expected %s~%s to be a near-duplicate, got score %.3f",
				label, items[i].ID, items[j].ID, score)
		}
	}
}

func TestNearDuplicateMemory_GroupAAllPairsDup(t *testing.T) {
	assertAllDup(t, "group A (vector dimension mismatch fact)", memtest.GroupANearDup())
}

func TestNearDuplicateMemory_GroupBAllPairsDup(t *testing.T) {
	assertAllDup(t, "group B (graph database ruled out fact)", memtest.GroupBNearDup())
}

func TestNearDuplicateMemory_GroupCAllPairsDup(t *testing.T) {
	assertAllDup(t, "group C (AppStatusChanged expected-noise pattern)", groupCNearDup())
}

// ── Cross-group: no pair across different groups may be a duplicate ──────

func assertNoCrossDup(t *testing.T, label string, xs, ys []memstore.Item) {
	t.Helper()
	for _, x := range xs {
		for _, y := range ys {
			score, dup := memstore.NearDuplicateMemory(x, y)
			assert.Falsef(t, dup, "%s: expected %s~%s NOT to be a near-duplicate, got score %.3f",
				label, x.ID, y.ID, score)
		}
	}
}

func TestNearDuplicateMemory_CrossGroupNotDup(t *testing.T) {
	a, b, c := memtest.GroupANearDup(), memtest.GroupBNearDup(), groupCNearDup()
	assertNoCrossDup(t, "A vs B", a, b)
	assertNoCrossDup(t, "A vs C", a, c)
	assertNoCrossDup(t, "B vs C", b, c)
}

// ── Distinct set: none may collapse into a group, or into each other ─────

func TestNearDuplicateMemory_DistinctNotDupWithGroups(t *testing.T) {
	a, b, c := memtest.GroupANearDup(), memtest.GroupBNearDup(), groupCNearDup()
	d := distinctNearDup()

	assertNoCrossDup(t, "postgres symptom vs A", []memstore.Item{d["d1_postgres_symptom"]}, a)
	assertNoCrossDup(t, "postgres symptom vs B", []memstore.Item{d["d1_postgres_symptom"]}, b)
	assertNoCrossDup(t, "postgres symptom vs C", []memstore.Item{d["d1_postgres_symptom"]}, c)

	assertNoCrossDup(t, "ingest OOM vs A", []memstore.Item{d["d2_oom_ingest"]}, a)
	assertNoCrossDup(t, "ingest OOM vs B", []memstore.Item{d["d2_oom_ingest"]}, b)
	assertNoCrossDup(t, "ingest OOM vs C", []memstore.Item{d["d2_oom_ingest"]}, c)
	assertNoCrossDup(t, "ingest OOM vs postgres symptom (different service tag)",
		[]memstore.Item{d["d2_oom_ingest"]}, []memstore.Item{d["d1_postgres_symptom"]})
	assertNoCrossDup(t, "ingest OOM vs untagged self-recover (different service tag on one side)",
		[]memstore.Item{d["d2_oom_ingest"]}, []memstore.Item{d["d4_untagged_selfrecover"]})

	assertNoCrossDup(t, "graph-db contradiction vs A", []memstore.Item{d["d3_graph_db_contradiction"]}, a)

	assertNoCrossDup(t, "untagged self-recover vs A (different kind)", []memstore.Item{d["d4_untagged_selfrecover"]}, a)
	assertNoCrossDup(t, "untagged self-recover vs B (different kind)", []memstore.Item{d["d4_untagged_selfrecover"]}, b)
	assertNoCrossDup(t, "untagged self-recover vs C (comparable: one side untagged, still not the same pattern)",
		[]memstore.Item{d["d4_untagged_selfrecover"]}, c)
}

// TestNearDuplicateMemory_GraphDbContradictionVsGroupB documents, rather than
// requires, one direction: D3 restates the same subject as group B ("graph
// database" + "search-api" + "root cause") but asserts the opposite
// conclusion. Token-set similarity cannot see negation, so this pair may
// legitimately score as a near-duplicate — which is the intended
// newest-statement-wins behavior at the write gate (a later "X was the
// cause after all" is meant to replace "X was ruled out" in place, not sit
// beside it). This test records whichever way the fixture actually falls,
// it does not force a direction.
func TestNearDuplicateMemory_GraphDbContradictionVsGroupB(t *testing.T) {
	d := distinctNearDup()
	contradiction := d["d3_graph_db_contradiction"]
	for _, b := range memtest.GroupBNearDup() {
		score, dup := memstore.NearDuplicateMemory(contradiction, b)
		t.Logf("contradiction(%s) vs %s: score=%.3f dup=%v", contradiction.ID, b.ID, score, dup)
	}
}

// ── Partition / kind / service isolation ─────────────────────────────────────

func TestNearDuplicateMemory_CrossPartitionNotDup(t *testing.T) {
	partitionA := memtest.DedupItem("t-partition-a", memstore.KindStableFact, "partition-a", "search-api", "active",
		"Vector store dimension mismatch confirmed as search-api root cause",
		"The search-api issue is rooted in a vector collection 'docs' having 768 dimensions while the embedding model requires 3072. This mismatch causes startup migration failures and continuous workload cycling, and has been operator-verified.")
	partitionB := memtest.DedupItem("t-partition-b", memstore.KindStableFact, "partition-b", "search-api", "active",
		partitionA.Title, partitionA.Content)

	score, dup := memstore.NearDuplicateMemory(partitionA, partitionB)
	assert.False(t, dup, "identical text under a different partition tag must not be a near-duplicate, got score %.3f", score)
	assert.False(t, memstore.DedupComparable(partitionA, partitionB), "items with different partition tags must not be comparable at all")
}

func TestNearDuplicateMemory_CrossKindNotDup(t *testing.T) {
	fact := memtest.DedupItem("k-fact", memstore.KindStableFact, "partition-a", "search-api", "active",
		"Vector store dimension mismatch confirmed as search-api root cause",
		"The search-api issue is rooted in a vector collection 'docs' having 768 dimensions while the embedding model requires 3072.")
	pattern := memtest.DedupItem("k-pattern", "operational_pattern", "partition-a", "search-api", "active",
		fact.Title, fact.Content)

	score, dup := memstore.NearDuplicateMemory(fact, pattern)
	assert.False(t, dup, "identical text under a different Kind must not be a near-duplicate, got score %.3f", score)
	assert.False(t, memstore.DedupComparable(fact, pattern), "items with different Kind must not be comparable at all")
}

func TestNearDuplicateMemory_DifferentServiceTagNotDup(t *testing.T) {
	svcA := memtest.DedupItem("s-a", memstore.KindStableFact, "partition-a", "search-api", "active",
		"Vector store dimension mismatch confirmed as root cause",
		"The issue is rooted in a vector collection 'docs' having 768 dimensions while the embedding model requires 3072.")
	svcB := memtest.DedupItem("s-b", memstore.KindStableFact, "partition-a", "billing-api", "active",
		svcA.Title, svcA.Content)

	score, dup := memstore.NearDuplicateMemory(svcA, svcB)
	assert.False(t, dup, "identical text under a different service/ tag must not be a near-duplicate, got score %.3f", score)
	assert.False(t, memstore.DedupComparable(svcA, svcB), "items with different service/ tags (both tagged) must not be comparable at all")
}

// ── Shared / author-tagged candidates are restricted merge targets ──────────
//
// A cell carrying memstore.SharedTag is a trust mark set only by an admin: it makes
// the cell visible to every partition, and an ordinary write must never
// rewrite that content in place through the dedup path. A cell carrying an
// author/<identity> tag marks its content as that identity's own claim, so
// it is only ever restated (merged into) by an incoming item stamped with
// the identical identity; a candidate must never be reported as a
// near-duplicate merge target for anyone else, even when its content
// genuinely near-duplicates the incoming item's.

func TestMemoryDedupComparable_SkipsSharedCandidate(t *testing.T) {
	candidate := memtest.GroupANearDup()[0]
	candidate.Tags = append(candidate.Tags, memstore.SharedTag)
	incoming := memtest.GroupANearDup()[1]

	assert.False(t, memstore.DedupComparable(candidate, incoming),
		"a candidate tagged shared must never be comparable as a merge target, even though its content near-duplicates the incoming item")
}

func TestMemoryDedupComparable_SkipsAuthorTaggedCandidate(t *testing.T) {
	candidate := memtest.GroupANearDup()[0]
	candidate.Tags = append(candidate.Tags, memstore.AuthorTagPrefix+"operator")
	incoming := memtest.GroupANearDup()[1]

	assert.False(t, memstore.DedupComparable(candidate, incoming),
		"a candidate tagged author/operator must never be comparable against an incoming item with no author tag, even though its content near-duplicates the incoming item")
}

func TestMemoryDedupComparable_SameAuthorCandidateMatchesOwnRestatement(t *testing.T) {
	candidate := memtest.GroupANearDup()[0]
	candidate.Tags = append(candidate.Tags, memstore.AuthorTagPrefix+"operator")
	incoming := memtest.GroupANearDup()[1]
	incoming.Tags = append(incoming.Tags, memstore.AuthorTagPrefix+"operator")

	assert.True(t, memstore.DedupComparable(candidate, incoming),
		"a candidate tagged author/operator must be comparable against an incoming item stamped with the identical author/operator tag")
}

func TestMemoryDedupComparable_DifferentAuthorCandidateNeverMatches(t *testing.T) {
	candidate := memtest.GroupANearDup()[0]
	candidate.Tags = append(candidate.Tags, memstore.AuthorTagPrefix+"operator")
	incoming := memtest.GroupANearDup()[1]
	incoming.Tags = append(incoming.Tags, memstore.AuthorTagPrefix+"other")

	assert.False(t, memstore.DedupComparable(candidate, incoming),
		"a candidate tagged author/operator must never be comparable against an incoming item stamped with a different author/other tag")
}

func TestMemoryDedupComparable_OrdinarySamePartitionCandidateStillMatches(t *testing.T) {
	candidate := memtest.GroupANearDup()[0]
	incoming := memtest.GroupANearDup()[1]

	assert.True(t, memstore.DedupComparable(candidate, incoming),
		"an ordinary same-partition/kind/service candidate carrying no shared or author tag must remain comparable")
}

func TestFindNearDuplicate_SkipsSharedCandidates(t *testing.T) {
	candidates := memtest.GroupANearDup()
	for i := range candidates {
		candidates[i].Tags = append(candidates[i].Tags, memstore.SharedTag)
	}
	query := memtest.DedupItem("new-a", memstore.KindStableFact, "partition-a", "search-api", "active",
		"Vector dimension mismatch is the confirmed search-api root cause",
		"Operator-verified: the search-api root cause is a vector collection 'docs' dimension mismatch, 768-dim vs the 3072-dim the embedding model requires, driving startup migration failures and workload cycling.")

	_, _, ok := memstore.FindNearDuplicate(candidates, query)
	assert.False(t, ok, "candidates tagged shared must never be returned as a near-duplicate merge target, even though their content matches the query")
}

func TestFindNearDuplicate_SkipsAuthorTaggedCandidates(t *testing.T) {
	candidates := memtest.GroupANearDup()
	for i := range candidates {
		candidates[i].Tags = append(candidates[i].Tags, memstore.AuthorTagPrefix+"operator")
	}
	query := memtest.DedupItem("new-a", memstore.KindStableFact, "partition-a", "search-api", "active",
		"Vector dimension mismatch is the confirmed search-api root cause",
		"Operator-verified: the search-api root cause is a vector collection 'docs' dimension mismatch, 768-dim vs the 3072-dim the embedding model requires, driving startup migration failures and workload cycling.")

	_, _, ok := memstore.FindNearDuplicate(candidates, query)
	assert.False(t, ok, "candidates tagged author/operator must never be returned as a near-duplicate merge target, even though their content matches the query")
}

// ── Short-text gate (NearDupMinTokens) ──────────────────────────────
//
// Below NearDupMinTokens tokens, Jaccard/containment on a handful of
// words is not a reliable signal: two short, DISTINCT facts can share
// enough vocabulary to clear memstore.MemoryNearDupThreshold by coincidence. This is
// the shape of two short, unrelated notes:
// "Deploy widget-api" / "content" vs
// "widget-api owns billing" / "content" — token sets {deploy, widget, api,
// content} and {widget, api, owns, billing, content}, Jaccard 3/6 = 0.5,
// which used to read as a duplicate under the 0.3 threshold alone.

func TestNearDuplicateMemory_ShortTextModerateOverlapNotDup(t *testing.T) {
	deploy := memtest.DedupItem("short-deploy", memstore.KindStableFact, "", "", "active",
		"Deploy widget-api", "content")
	owns := memtest.DedupItem("short-owns", memstore.KindStableFact, "", "", "active",
		"widget-api owns billing", "content")

	require.Less(t, len(memstore.DedupTokens(deploy)), memstore.NearDupMinTokens, "fixture must exercise the short-text gate")
	require.Less(t, len(memstore.DedupTokens(owns)), memstore.NearDupMinTokens, "fixture must exercise the short-text gate")

	score, dup := memstore.NearDuplicateMemory(deploy, owns)
	t.Logf("short-text moderate overlap: score=%.3f dup=%v", score, dup)
	assert.False(t, dup, "two short, distinct facts with moderate token overlap must not collapse")
}

func TestNearDuplicateMemory_ShortIdenticalTextIsDup(t *testing.T) {
	a := memtest.DedupItem("short-id-a", memstore.KindStableFact, "", "", "active",
		"Deploy widget-api", "content")
	b := memtest.DedupItem("short-id-b", memstore.KindStableFact, "", "", "active",
		"Deploy widget-api", "content")

	require.Less(t, len(memstore.DedupTokens(a)), memstore.NearDupMinTokens, "fixture must exercise the short-text gate")

	score, dup := memstore.NearDuplicateMemory(a, b)
	t.Logf("short identical text: score=%.3f dup=%v", score, dup)
	assert.True(t, dup, "two short items with identical text must still collapse as near-identical")
}

func TestNearDuplicateMemory_EightTokenParaphraseStillDup(t *testing.T) {
	// Both sides at or above memstore.NearDupMinTokens, worded as a paraphrase
	// rather than identical text: the ordinary threshold/containment path
	// must still apply once both token sets clear the short-text gate.
	a := memtest.DedupItem("min-tok-a", memstore.KindStableFact, "", "", "active",
		"Billing export job fails intermittently",
		"The billing export batch job intermittently fails during the nightly run because of a database connection timeout.")
	b := memtest.DedupItem("min-tok-b", memstore.KindStableFact, "", "", "active",
		"Nightly billing export batch job intermittently failing",
		"The nightly billing export batch job fails intermittently due to a database connection timeout during the run.")

	require.GreaterOrEqual(t, len(memstore.DedupTokens(a)), memstore.NearDupMinTokens, "fixture must clear the short-text gate")
	require.GreaterOrEqual(t, len(memstore.DedupTokens(b)), memstore.NearDupMinTokens, "fixture must clear the short-text gate")

	score, dup := memstore.NearDuplicateMemory(a, b)
	t.Logf("8+-token paraphrase: score=%.3f dup=%v", score, dup)
	assert.True(t, dup, "an 8+-token paraphrase pair must still collapse via the ordinary threshold/containment path")
}

// TestNearDuplicateMemory_FixtureGroupsClearMinTokens is the sanity check
// the short-text gate requires of the existing group fixtures: every member
// must stay at or above memstore.NearDupMinTokens tokens, or the group-internal
// "all pairs dup" assertions above would need the short-text path instead of
// the ordinary one.
func TestNearDuplicateMemory_FixtureGroupsClearMinTokens(t *testing.T) {
	check := func(label string, items []memstore.Item) {
		for _, it := range items {
			n := len(memstore.DedupTokens(it))
			assert.GreaterOrEqualf(t, n, memstore.NearDupMinTokens, "%s: %s has only %d tokens, below memstore.NearDupMinTokens", label, it.ID, n)
		}
	}
	check("group A", memtest.GroupANearDup())
	check("group B", memtest.GroupBNearDup())
	check("group C", groupCNearDup())
}

// ── memstore.FindNearDuplicate ──────────────────────────────────────────────────────

func TestFindNearDuplicate(t *testing.T) {
	candidates := memtest.GroupANearDup()
	query := memtest.DedupItem("new-a", memstore.KindStableFact, "partition-a", "search-api", "active",
		"Vector dimension mismatch is the confirmed search-api root cause",
		"Operator-verified: the search-api root cause is a vector collection 'docs' dimension mismatch, 768-dim vs the 3072-dim the embedding model requires, driving startup migration failures and workload cycling.")

	best, score, ok := memstore.FindNearDuplicate(candidates, query)
	require.True(t, ok, "expected a near-duplicate among group A candidates")
	assert.GreaterOrEqual(t, score, memstore.MemoryNearDupThreshold)
	t.Logf("memstore.FindNearDuplicate matched %s with score %.3f", best.ID, score)

	found := false
	for _, c := range candidates {
		if c.ID == best.ID {
			found = true
		}
	}
	assert.True(t, found, "best match must be one of the candidates")
}

func TestFindNearDuplicate_IgnoresSelf(t *testing.T) {
	candidates := memtest.GroupANearDup()
	// Every group-A item legitimately duplicates its siblings, so querying
	// with a candidate that is itself present in the list is still expected
	// to find a match (one of the OTHER siblings) — what must never happen
	// is the candidate matching itself by ID.
	for _, query := range candidates {
		best, _, ok := memstore.FindNearDuplicate(candidates, query)
		if ok {
			assert.NotEqual(t, query.ID, best.ID, "an item must never be reported as its own near-duplicate")
		}
	}

	// A singleton candidate list containing only the query itself must find
	// no duplicate at all.
	self := candidates[0]
	_, _, ok := memstore.FindNearDuplicate(candidates[:1], self)
	assert.False(t, ok, "with only itself in the candidate list, an item must find no near-duplicate")
}

func TestFindNearDuplicate_IgnoresInactiveCandidates(t *testing.T) {
	candidates := memtest.GroupANearDup()
	for i := range candidates {
		candidates[i].Status = "superseded"
	}
	query := memtest.DedupItem("new-a", memstore.KindStableFact, "partition-a", "search-api", "active",
		candidates[0].Title, candidates[0].Content)

	_, _, ok := memstore.FindNearDuplicate(candidates, query)
	assert.False(t, ok, "a superseded candidate must never be returned as a near-duplicate to update in place")
}

// ── memstore.ClusterNearDuplicates ──────────────────────────────────────────────────

func TestClusterNearDuplicates_GroupsClusterSeparately(t *testing.T) {
	a, b, c := memtest.GroupANearDup(), memtest.GroupBNearDup(), groupCNearDup()

	// Interleave so a naive "adjacent items only" implementation would fail
	// while the greedy first-member-of-each-cluster algorithm still
	// succeeds.
	var items []memstore.Item
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	if len(c) > max {
		max = len(c)
	}
	for i := 0; i < max; i++ {
		if i < len(a) {
			items = append(items, a[i])
		}
		if i < len(b) {
			items = append(items, b[i])
		}
		if i < len(c) {
			items = append(items, c[i])
		}
	}

	clusters := memstore.ClusterNearDuplicates(items)
	require.Len(t, clusters, 3, "expected exactly one cluster per fact/pattern group")

	byID := func(cluster []memstore.Item) map[string]bool {
		m := make(map[string]bool, len(cluster))
		for _, it := range cluster {
			m[it.ID] = true
		}
		return m
	}

	var gotA, gotB, gotC map[string]bool
	for _, cluster := range clusters {
		ids := byID(cluster)
		switch {
		case ids["a1"]:
			gotA = ids
		case ids["b1"]:
			gotB = ids
		case ids["c1"]:
			gotC = ids
		}
	}

	require.NotNil(t, gotA)
	require.NotNil(t, gotB)
	require.NotNil(t, gotC)
	assert.Len(t, gotA, len(a))
	assert.Len(t, gotB, len(b))
	assert.Len(t, gotC, len(c))
	for _, it := range a {
		assert.True(t, gotA[it.ID], "expected %s in the group A cluster", it.ID)
	}
	for _, it := range b {
		assert.True(t, gotB[it.ID], "expected %s in the group B cluster", it.ID)
	}
	for _, it := range c {
		assert.True(t, gotC[it.ID], "expected %s in the group C cluster", it.ID)
	}
}

func TestClusterNearDuplicates_SingleItemIsItsOwnCluster(t *testing.T) {
	clusters := memstore.ClusterNearDuplicates(memtest.GroupANearDup()[:1])
	require.Len(t, clusters, 1)
	assert.Len(t, clusters[0], 1)
}

func TestClusterNearDuplicates_Empty(t *testing.T) {
	clusters := memstore.ClusterNearDuplicates(nil)
	assert.Empty(t, clusters)
}
