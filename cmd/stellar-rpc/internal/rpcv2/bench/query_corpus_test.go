package bench

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdkingest "github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/adapters"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/rpcv2test"
)

func TestCorpusSpansManyLedgersOnADenseDataset(t *testing.T) {
	const txPerLedger = 4 * corpusMaxHashesPerLedger
	f, release := openDenseHotFixture(t, 64, txPerLedger)
	defer release()

	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	s := newTxHashSampler(testRNG())
	require.NoError(t, s.sampleChunk(view, f.Chunks[0], f.FirstLedger, f.LastLedger))

	assert.GreaterOrEqual(t, len(s.hashes), corpusTargetHashes, "the pool did not fill")
	assert.GreaterOrEqual(t, len(s.ledgers), corpusTargetHashes/corpusMaxHashesPerLedger,
		"the pool covers too few ledgers")

	perLedger := hashesPerLedger(t, f, view, s.hashes)
	for seq, n := range perLedger {
		assert.LessOrEqual(t, n, corpusMaxHashesPerLedger, "ledger %d is over the per-ledger cap", seq)
	}
	seqs := make([]uint32, 0, len(perLedger))
	for seq := range perLedger {
		seqs = append(seqs, seq)
	}
	assert.ElementsMatch(t, s.ledgers, seqs, "the pool's hashes come from the ledgers the sampler listed")
	assert.Len(t, uniqueHashes(s.hashes), len(s.hashes), "a hash is sampled at most once")
}

func TestCorpusTakesEveryHashOfALedgerBelowTheCap(t *testing.T) {
	const txPerLedger = corpusMaxHashesPerLedger / 2
	f, release := openDenseHotFixture(t, 2*corpusTargetHashes/txPerLedger, txPerLedger)
	defer release()

	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	s := newTxHashSampler(testRNG())
	require.NoError(t, s.sampleChunk(view, f.Chunks[0], f.FirstLedger, f.LastLedger))

	assert.GreaterOrEqual(t, len(s.hashes), corpusTargetHashes, "the pool did not fill")
	for seq, n := range hashesPerLedger(t, f, view, s.hashes) {
		assert.Equal(t, txPerLedger, n, "ledger %d did not contribute its whole transaction set", seq)
	}
	assert.Len(t, uniqueHashes(s.hashes), len(s.hashes), "a hash is sampled at most once")
}

func TestCorpusSkipsLedgersWithoutTransactions(t *testing.T) {
	const numLedgers = 2 * eventEvery
	packDir, txLedgers := writeSourcePack(t, t.TempDir(), chunk.ID(0), numLedgers)
	f, release := openHotFixtureOverPack(t, packDir, numLedgers)
	defer release()

	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	s := newTxHashSampler(testRNG())
	require.NoError(t, s.sampleChunk(view, f.Chunks[0], f.FirstLedger, f.LastLedger))

	require.NotEmpty(t, s.hashes)
	assert.LessOrEqual(t, len(s.ledgers), txLedgers, "only tx-bearing ledgers may contribute")
	assert.Len(t, s.hashes, len(s.ledgers), "each of these ledgers holds one transaction")
	for _, seq := range s.ledgers {
		assert.Zero(t, (seq-f.FirstLedger)%eventEvery, "ledger %d carries no transaction", seq)
	}
}

func TestSampleHashesFromLedgerDrawsARandomSubset(t *testing.T) {
	parts := make([]sdkingest.LedgerTxParts, 64)
	for i := range parts {
		parts[i].Hash[0] = byte(i)
	}

	first := sampleHashesFromLedger(rand.New(rand.NewPCG(1, 1)), parts)
	second := sampleHashesFromLedger(rand.New(rand.NewPCG(2, 2)), parts)
	require.Len(t, first, corpusMaxHashesPerLedger)
	require.Len(t, second, corpusMaxHashesPerLedger)
	assert.NotEqual(t, first, second, "two seeds must draw different subsets")
	assert.Len(t, uniqueHashes(first), len(first), "a draw takes each transaction at most once")

	inOrder := make([][32]byte, 0, corpusMaxHashesPerLedger)
	for _, p := range parts[:corpusMaxHashesPerLedger] {
		inOrder = append(inOrder, p.Hash)
	}
	assert.NotEqual(t, inOrder, first, "the draw is not the ledger's first transactions")

	few := parts[:3]
	whole := [][32]byte{few[0].Hash, few[1].Hash, few[2].Hash}
	assert.ElementsMatch(t, whole, sampleHashesFromLedger(testRNG(), few))
}

func TestCorpusLogsItsLedgerCoverage(t *testing.T) {
	t.Run("many ledgers", func(t *testing.T) {
		f, release := openDenseHotFixture(t, 8, 20)
		defer release()

		info, warnings := buildCorpusCapturingLogs(t, f)
		assert.Contains(t, info, "hashes over 8 ledgers spanning 2..9")
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "txhash corpus underfilled:")
	})

	t.Run("one ledger", func(t *testing.T) {
		f, release := openDenseHotFixture(t, 1, 20)
		defer release()

		info, warnings := buildCorpusCapturingLogs(t, f)
		assert.Contains(t, info, "hashes over 1 ledgers spanning 2..2")
		require.Len(t, warnings, 2)
		assert.Contains(t, warnings[0], "came from ledger 2 alone")
		assert.Contains(t, warnings[1], "txhash corpus underfilled: 16 of 512 requested hashes")
	})
}

// buildCorpusCapturingLogs builds the tx-hash corpus over f and returns the
// coverage line it logged and the warnings it raised.
func buildCorpusCapturingLogs(t *testing.T, f *queryFixture) (string, []string) {
	t.Helper()
	logger := testLogger()
	done := logger.StartTest(logrus.InfoLevel)
	_, err := buildTxHashCorpus(context.Background(), logger, f, 0.1, defaultSeed, 0)
	entries := done()
	require.NoError(t, err)

	var coverage string
	for _, m := range logMessages(entries, logrus.InfoLevel) {
		if strings.HasPrefix(m, "txhash corpus:") {
			coverage = m
		}
	}
	require.NotEmpty(t, coverage, "the corpus build logs its coverage")
	return coverage, logMessages(entries, logrus.WarnLevel)
}

func TestVerifySampledHashReportsThePassphrase(t *testing.T) {
	f, release := openDenseHotFixture(t, 4, 4)
	defer release()

	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	s := newTxHashSampler(testRNG())
	require.NoError(t, s.sampleChunk(view, f.Chunks[0], f.FirstLedger, f.LastLedger))
	require.NotEmpty(t, s.hashes)
	hash, seq := s.first()

	t.Run("the configured passphrase resolves it", func(t *testing.T) {
		require.NoError(t, verifySampledHashResolves(context.Background(), view, f, hash, seq))
	})

	t.Run("wrong passphrase", func(t *testing.T) {
		wrong := *f
		wrong.Passphrase = network.TestNetworkPassphrase
		err := verifySampledHashResolves(context.Background(), view, &wrong, hash, seq)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--network-passphrase")
		assert.NotContains(t, err.Error(), "probe of a known transaction hash failed")
	})
}

func TestBuildTxHashCorpusExactTarget(t *testing.T) {
	f, release := openDenseHotFixture(t, 128, 32)
	defer release()
	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	for _, target := range []int{1, 7, 17, 0, 513, 1025} {
		t.Run(fmt.Sprintf("target=%d", target), func(t *testing.T) {
			want := target
			if target == 0 {
				want = corpusTargetHashes
			}
			first, err := buildTxHashCorpus(context.Background(), testLogger(), f, 0.25, defaultSeed, target)
			require.NoError(t, err)
			require.Len(t, first.hashes, want, "the last ledger must be trimmed to the exact target")
			assert.Len(t, uniqueHashes(first.hashes), want)
			assert.InDelta(t, 0.25, first.missFraction, 1e-12)
			counts := hashesPerLedger(t, f, view, first.hashes)
			assert.Equal(t, len(counts), first.ledgerCount, "metadata counts contributing ledgers")
			assert.Equal(t, (want+15)/16, first.ledgerCount)
			for seq, count := range counts {
				assert.LessOrEqual(t, count, 16, "ledger %d", seq)
			}
			repeated, err := buildTxHashCorpus(context.Background(), testLogger(), f, 0.25, defaultSeed, target)
			require.NoError(t, err)
			assert.Equal(t, first, repeated, "the same seed reproduces hash order and metadata")
			other, err := buildTxHashCorpus(context.Background(), testLogger(), f, 0.25, defaultSeed+1, target)
			require.NoError(t, err)
			assert.NotEqual(t, first.hashes, other.hashes)
		})
	}
}

func TestBuildTxHashCorpusUnderfilled(t *testing.T) {
	f, release := openDenseHotFixture(t, 1, 3)
	defer release()
	logger := testLogger()
	done := logger.StartTest(logrus.WarnLevel)
	corpus, err := buildTxHashCorpus(context.Background(), logger, f, 0, defaultSeed, 17)
	entries := done()
	require.NoError(t, err)
	assert.Len(t, corpus.hashes, 3)
	assert.Equal(t, 1, corpus.ledgerCount)
	assert.Contains(t, strings.Join(logMessages(entries, logrus.WarnLevel), "\n"),
		"txhash corpus underfilled: 3 of 17 requested hashes")
	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()
	assert.Equal(t, map[uint32]int{f.FirstLedger: 3}, hashesPerLedger(t, f, view, corpus.hashes))
}

func TestVerifySampledHashReportsAProbeFailure(t *testing.T) {
	chunkID := chunk.ID(0)
	coldRoot := ingestColdChunk(t, chunkID)
	require.NoError(t, os.RemoveAll(geometry.NewLayout(coldRoot).TxHashIndexRoot()))

	// With txhash absent from Types the open warns about the missing index and
	// does not fail.
	f, release, err := openColdFixture(testLogger(), coldQueryOptions{
		ColdRoot:   coldRoot,
		StartChunk: chunkID,
		NumChunks:  1,
		Plan:       queryPlan{Types: []string{queryTypeLedgers}, Passphrase: network.PublicNetworkPassphrase},
	})
	require.NoError(t, err)
	defer release()

	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	s := newTxHashSampler(testRNG())
	require.NoError(t, s.sampleChunk(view, chunkID, f.FirstLedger, f.LastLedger))
	require.NotEmpty(t, s.hashes)
	hash, seq := s.first()

	err = verifySampledHashResolves(context.Background(), view, f, hash, seq)
	require.ErrorContains(t, err, "probe of a known transaction hash failed")
	assert.Contains(t, err.Error(), "tx-hash index may be missing")
	assert.NotContains(t, err.Error(), "--network-passphrase")
}

// hashesPerLedger resolves each hash through the served by-hash path and counts
// the hashes per ledger.
func hashesPerLedger(
	t *testing.T, f *queryFixture, view *query.ReadView, hashes [][32]byte,
) map[uint32]int {
	t.Helper()
	reader := adapters.NewTransactionReader(f.Passphrase, nil)
	ctx := query.WithView(context.Background(), view)
	counts := map[uint32]int{}
	for _, h := range hashes {
		tx, err := reader.GetTransaction(ctx, xdr.Hash(h))
		require.NoError(t, err, "sampled hash %x does not resolve", h)
		counts[tx.Ledger.Sequence]++
	}
	return counts
}

func uniqueHashes(hashes [][32]byte) map[[32]byte]struct{} {
	set := make(map[[32]byte]struct{}, len(hashes))
	for _, h := range hashes {
		set[h] = struct{}{}
	}
	return set
}

// openDenseHotFixture ingests numLedgers ledgers of txPerLedger transactions
// each into a hot database and opens the query fixture over it.
func openDenseHotFixture(t *testing.T, numLedgers uint32, txPerLedger int) (*queryFixture, func()) {
	t.Helper()
	packDir := writeLedgerPack(t, t.TempDir(), chunk.ID(0), numLedgers, func(seq uint32) []byte {
		return rpcv2test.MultiTxLCMBytes(t, seq, txPerLedger)
	})
	return openHotFixtureOverPack(t, packDir, numLedgers)
}

// openHotFixtureOverPack ingests numLedgers ledgers of chunk 0 from packDir
// into a fresh hot database and opens the query fixture over it under the
// pubnet passphrase.
func openHotFixtureOverPack(t *testing.T, packDir string, numLedgers uint32) (*queryFixture, func()) {
	t.Helper()
	f, release, err := openHotFixture(testLogger(), hotQueryOptions{
		HotRoot: ingestHotChunk(t, packDir, numLedgers),
		Chunk:   chunk.ID(0),
		Plan:    queryPlan{Passphrase: network.PublicNetworkPassphrase},
	})
	require.NoError(t, err)
	return f, release
}
