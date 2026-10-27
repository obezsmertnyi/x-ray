package memstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func TestResolveOrigin(t *testing.T) {
	cases := []struct {
		name        string
		existing    string
		hasExisting bool
		incoming    string
		want        string
		wantMsg     string
	}{
		{"new item without origin is derived", "", false, "", OriginDerived, ""},
		{"empty incoming keeps the stored origin", OriginObserved, true, "", OriginObserved, ""},
		{"incoming origin wins over a derived cell", OriginDerived, true, OriginHuman, OriginHuman, ""},
		{"derived cannot replace human", OriginHuman, true, OriginDerived, "", errDerivedReplacesHuman},
		{"empty incoming onto a human cell keeps it human", OriginHuman, true, "", OriginHuman, ""},
		{"a stored cell with no origin and an empty write is derived", "", true, "", OriginDerived, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg := resolveOrigin(c.existing, c.hasExisting, c.incoming)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.wantMsg, msg)
		})
	}
}

func TestRestoredOrigin(t *testing.T) {
	spec := func(origin string, tags ...string) Item {
		return Item{MemoryCellSpec: memoryv1.MemoryCellSpec{Origin: origin, Tags: tags}}
	}
	assert.Equal(t, OriginObserved, RestoredOrigin(spec(OriginObserved)), "a stored origin is kept")
	assert.Equal(t, OriginHuman, RestoredOrigin(spec("", "author/alice")), "a cell written before the field with an author tag is human")
	assert.Equal(t, OriginDerived, RestoredOrigin(spec("", "topic/x")))
	assert.Equal(t, OriginDerived, RestoredOrigin(spec("")))
}
