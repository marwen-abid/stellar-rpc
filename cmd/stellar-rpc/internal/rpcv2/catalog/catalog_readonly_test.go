package catalog

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/rpcv2test/fileset"
)

// writeCatalog opens a catalog read-write at path, applies mutate, closes it
// and returns the secret that the open minted.
func writeCatalog(t *testing.T, path string, mutate func(c *Catalog)) [32]byte {
	t.Helper()
	c, err := openKVAt(t, path)
	require.NoError(t, err)
	mutate(c)
	secret := c.Secret()
	require.NoError(t, c.Close())
	return secret
}

func openReadOnlyAt(t *testing.T, path string) (*Catalog, error) {
	t.Helper()
	idxLayout, err := geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)
	require.NoError(t, err)
	c, err := OpenReadOnly(path, geometry.NewLayout(t.TempDir()), idxLayout, silentLogger())
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func TestOpenReadOnly_FileSetUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rocksdb")
	secret := writeCatalog(t, path, func(c *Catalog) {
		require.NoError(t, c.PinEarliestLedger(chunk.ID(1).FirstLedger()))
		require.NoError(t, c.FlipHotReady(chunk.ID(2)))
	})
	before := fileset.Take(t, path)

	c, err := openReadOnlyAt(t, path)
	require.NoError(t, err)

	earliest, ok, err := c.EarliestLedger()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, chunk.ID(1).FirstLedger(), earliest)

	ready, err := c.ReadyHotChunkKeys()
	require.NoError(t, err)
	assert.Equal(t, []chunk.ID{2}, ready)

	state, err := c.HotState(chunk.ID(2))
	require.NoError(t, err)
	assert.Equal(t, geometry.HotReady, state)

	snap, err := c.NewSnapshot()
	require.NoError(t, err)
	_, err = snap.LastCompleteChunk()
	require.NoError(t, err)
	snap.Release()

	assert.Equal(t, secret, c.Secret())

	require.NoError(t, c.Close())
	fileset.RequireUnchanged(t, path, before)
}

func TestOpenReadOnly_MissingCatalog(t *testing.T) {
	p := t.TempDir()
	before := fileset.Take(t, p)

	_, err := openReadOnlyAt(t, filepath.Join(p, "catalog", "rocksdb"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	fileset.RequireUnchanged(t, p, before)
}

func TestOpenReadOnly_RejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rocksdb")
	writeCatalog(t, path, func(*Catalog) {})
	before := fileset.Take(t, path)

	c, err := openReadOnlyAt(t, path)
	require.NoError(t, err)
	require.Error(t, c.FlipHotReady(chunk.ID(2)))
	require.NoError(t, c.Close())
	fileset.RequireUnchanged(t, path, before)
}

func TestOpenReadOnly_RefusesForeignKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rocksdb")
	writeCatalog(t, path, func(c *Catalog) {
		require.NoError(t, c.put("format:events", "2"))
	})

	_, err := openReadOnlyAt(t, path)
	require.ErrorIs(t, err, ErrForeignCatalog)
}

func TestOpenReadOnly_NoSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rocksdb")
	writeCatalog(t, path, func(c *Catalog) {
		require.NoError(t, c.del(catalogSecretStoreKey))
	})

	_, err := openReadOnlyAt(t, path)
	require.ErrorContains(t, err, "catalog: no cold-index secret at "+path)
}
