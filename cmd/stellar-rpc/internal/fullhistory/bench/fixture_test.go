package bench

import (
	"crypto/sha256"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
)

// packDigest generates a fixture pack and returns its content hash.
func packDigest(t *testing.T, seed uint64) [sha256.Size]byte {
	t.Helper()
	root := t.TempDir()
	txLedgers, err := writeFixturePack(root, chunk.ID(0), 3*eventEvery, seed)
	require.NoError(t, err)
	require.Equal(t, 3, txLedgers)
	raw, err := os.ReadFile(geometry.LedgerPackPath(root, chunk.ID(0)))
	require.NoError(t, err)
	return sha256.Sum256(raw)
}

// TestFixturePackDeterministic: the same (chunk, numLedgers, seed) must
// produce byte-identical packs — that is what makes the CI dataset a defined
// dataset — and a different seed must not.
func TestFixturePackDeterministic(t *testing.T) {
	first := packDigest(t, 1)
	again := packDigest(t, 1)
	assert.Equal(t, first, again)
	assert.NotEqual(t, first, packDigest(t, 2))
}
