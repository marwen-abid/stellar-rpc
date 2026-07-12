package bench

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventCorpusDraw checks the draw invariants over many draws: no empty
// (accidental match-all) filters, no filter count beyond the term universe,
// and every constraint value comes from the corpus.
func TestEventCorpusDraw(t *testing.T) {
	corpus := &eventCorpus{terms: []eventTerm{
		{category: 0, value: []byte("contract-a")},
		{category: 0, value: []byte("contract-b")},
		{category: 1, value: []byte("topic0-x")},
		{category: 1, value: []byte("topic0-y")},
		{category: 3, value: []byte("topic2-z")},
	}}
	known := make(map[string]bool, len(corpus.terms))
	for _, term := range corpus.terms {
		known[string(term.value)] = true
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		filters := corpus.draw(rng)
		require.NotEmpty(t, filters)
		require.LessOrEqual(t, len(filters), len(corpus.terms))
		for i := range filters {
			require.False(t, filterIsEmpty(&filters[i]), "draw produced a match-all filter")
			if len(filters[i].ContractID) > 0 {
				assert.True(t, known[string(filters[i].ContractID)])
			}
			for _, topic := range filters[i].Topics {
				if len(topic) > 0 {
					assert.True(t, known[string(topic)])
				}
			}
		}
	}
}

// TestPageCursorsDraw pins the prefix-sum position mapping: draws land on
// transaction-bearing ledgers at in-ledger indexes that leave a full page
// ahead.
func TestPageCursorsDraw(t *testing.T) {
	// Ledgers first+0..3 hold 0, 5, 0, 2 transactions.
	p := &pageCursors{first: 10, starts: []int{0, 0, 5, 5}, total: 7}
	const pageSize = 2
	rng := rand.New(rand.NewPCG(3, 4))
	seen := make(map[[2]int]bool)
	for range 200 {
		seq, skip := p.draw(rng, pageSize)
		switch seq {
		case 11:
			assert.Less(t, skip, 5)
		case 13:
			assert.Equal(t, 0, skip)
		default:
			t.Fatalf("draw landed on ledger %d, which has no transactions", seq)
		}
		// The global position must leave pageSize transactions ahead.
		pos := p.starts[seq-p.first] + skip
		assert.LessOrEqual(t, pos, p.total-pageSize)
		seen[[2]int{int(seq), skip}] = true
	}
	assert.Greater(t, len(seen), 1, "draw should spread over positions")
}
