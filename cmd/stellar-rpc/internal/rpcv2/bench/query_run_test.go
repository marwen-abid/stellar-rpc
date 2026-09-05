package bench

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLegSeedIsDistinctPerType(t *testing.T) {
	seen := map[int64]string{}
	for _, qtype := range allQueryTypes {
		seed := legSeed(defaultSeed, qtype)
		if prev, dup := seen[seed]; dup {
			t.Fatalf("%s and %s share leg seed %d", prev, qtype, seed)
		}
		seen[seed] = qtype
	}
	assert.Len(t, seen, len(allQueryTypes))
}
