package index

import (
	"crypto/md5"
	"encoding/binary"
	"math"
	"testing"

	"github.com/rs/zerolog"
)

// sampleEvent is a record whose message is templated: the same text goes out
// from several deployments with only the environment token changed, which makes
// their embeddings sit close together.
type sampleEvent struct {
	source, service, disposition, groupID, message string
}

var sampleEvents = []sampleEvent{
	{
		source: "app/dev/dev/billing-service", service: "billing-service",
		disposition: "new_report", groupID: "billing-service-degraded-dev-1",
		message: "Application billing-service degraded (2/3): 0/1 workloads not ready; components not ready: Deployment/dev-shop-billing-service: InProgress",
	},
	{
		source: "app/prod/prod/billing-service", service: "billing-service",
		disposition: "new_report", groupID: "billing-service-degraded-prod-1",
		message: "Application billing-service degraded (2/3): 0/1 workloads not ready; components not ready: Deployment/prod-shop-billing-service: InProgress",
	},
	{
		source: "app/stable/stable/billing-service", service: "billing-service",
		disposition: "updated_report", groupID: "billing-service-stable-001",
		message: "Application billing-service degraded (2/3): 0/1 workloads not ready; components not ready: Deployment/stable-shop-billing-service: InProgress",
	},
	{
		source: "app/ops/report-service", service: "report-service",
		disposition: "updated_report", groupID: "ops-billing-service",
		message: "Application report-service degraded (2/3): 1 components not ready; components not ready: Deployment/ops-shop-report-service: InProgress",
	},
}

// embed returns a deterministic, network-free character-trigram embedding of
// "source + message": near-identical strings give near-identical vectors, which
// is the collision shape the guards under test exist for.
func embed(t *testing.T, source, message string) []float32 {
	t.Helper()
	return detEmbed(source + " " + message)
}

func detEmbed(s string) []float32 {
	const dim = 256
	v := make([]float32, dim)
	for i := 0; i+3 <= len(s); i++ {
		sum := md5.Sum([]byte(s[i : i+3]))
		idx := binary.BigEndian.Uint64(sum[:8]) % dim
		v[idx]++
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func find(t *testing.T, source string) sampleEvent {
	t.Helper()
	for _, e := range sampleEvents {
		if e.source == source {
			return e
		}
	}
	t.Fatalf("no sample event for source %s", source)
	return sampleEvent{}
}

// unitAt returns a unit vector whose cosine with (1, 0, 0, 0) is cos. Four
// dimensions: the cosine kernel needs a length divisible by 4.
func unitAt(cos float64) []float32 {
	return []float32{float32(cos), float32(math.Sqrt(1 - cos*cos)), 0, 0}
}

// newRawStore is a store with the semantic score gap off: the host decides what
// to cut, as the tests ported from there assume.
func newRawStore(e *Embedder) *CellStore {
	s := NewCellStore(e, zerolog.Nop())
	s.SetScoreGap(0)
	return s
}
