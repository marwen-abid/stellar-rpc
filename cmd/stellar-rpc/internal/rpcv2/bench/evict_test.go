package bench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A missing artifact is skipped, not an error, on every platform. evictFile
// wraps its open error, so the check must unwrap.
func TestEvictColdArtifactsSkipsMissingFile(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.pack")
	require.NoError(t, os.WriteFile(present, []byte("ledgers"), 0o600))
	missing := filepath.Join(dir, "missing.pack")

	f := &queryFixture{EvictPaths: []string{missing, present}}
	evicted, err := f.evictColdArtifacts()
	require.NoError(t, err)
	if evictSupported {
		assert.Equal(t, 1, evicted, "the present file is advised, the missing one skipped")
	} else {
		assert.Equal(t, 2, evicted, "off Linux the pass is a no-op that counts every path")
	}
}
