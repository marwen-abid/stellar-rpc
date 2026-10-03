package bench

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
)

// ingestHotChunk writes numLedgers ledgers of chunk 0 into a hot database and
// returns its --hot-dir.
func ingestHotChunk(t *testing.T, numLedgers uint32) string {
	t.Helper()
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, numLedgers)
	hotRoot := t.TempDir()
	require.NoError(t, runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		NumLedgers: numLedgers,
		HotRoot:    hotRoot,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}))
	return hotRoot
}

// Every query type reads under a live context. The ledgers and txpage scans
// stop with the context error once the context is done.
func TestQueryRequests(t *testing.T) {
	hotRoot := ingestHotChunk(t, 2*eventEvery)
	plan := queryPlan{
		Types:          allQueryTypes,
		LedgersSpan:    defaultLedgersSpan,
		TxPageSpan:     defaultTxPageSpan,
		TxPageLimit:    defaultTxPageLimit,
		EventsLimit:    defaultEventsLimit,
		Passphrase:     network.PublicNetworkPassphrase,
		Seed:           defaultSeed,
		TxHashPoolSize: poolTargetHashes,
	}
	f, release, err := openHotDataset(testLogger(), hotQueryOptions{HotRoot: hotRoot, Chunk: 0, Plan: plan})
	require.NoError(t, err)
	defer release()

	for _, qtype := range plan.Types {
		t.Run(qtype, func(t *testing.T) {
			req, err := newQueryRequest(context.Background(), testLogger(), f, plan, qtype)
			require.NoError(t, err)
			rng := rand.New(rand.NewPCG(defaultSeed, defaultSeed))
			for range 10 {
				timing, err := req(context.Background(), rng)
				require.NoError(t, err)
				switch qtype {
				case queryTypeLedgers:
					assert.Equal(t, defaultLedgersSpan, timing.items)
				case queryTypeTxHash:
					assert.Equal(t, 1, timing.items, "with no not-found lookups every lookup finds its transaction")
					assert.Equal(t, outcomeFound, timing.outcome)
				}
			}
		})
	}

	for _, qtype := range []string{queryTypeLedgers, queryTypeTxPage} {
		t.Run(qtype+" stops on cancel", func(t *testing.T) {
			req, err := newQueryRequest(context.Background(), testLogger(), f, plan, qtype)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = req(ctx, rand.New(rand.NewPCG(defaultSeed, defaultSeed)))
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
