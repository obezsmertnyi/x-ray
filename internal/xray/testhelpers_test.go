package xray

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
)

// backdate sets the creation time of a stored item, so a test can order or age
// items without waiting.
func backdate(s *MemoryStore, id string, t time.Time) {
	s.eng.Mutate(id, func(it *memstore.Item) bool {
		it.CreatedAt = metav1.NewTime(t)
		return true
	})
}
