package bench

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/ingest"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
)

// testQueryKnobs returns fixture-sized workload knobs: the fixture chunk
// carries one 1-tx event ledger per eventEvery, so pages and hash pools must
// stay small.
func testQueryKnobs(workers []int) queryKnobs {
	return queryKnobs{
		Types:          queryTypes{Ledgers: true, TxPage: true, Txhash: true, Events: true},
		Workers:        workers,
		Iters:          3,
		Seed:           1,
		LedgersPerRead: 20,
		PageSize:       5,
		MaxEvents:      1000,
		SampleLedgers:  int(chunk.LedgersPerChunk),
		Passphrase:     network.PublicNetworkPassphrase,
	}
}

// TestRunQueryColdFromArtifacts is the end-to-end cold path: fabricate a
// full fixture chunk, freeze it through the production cold ingest
// (bench-ingest's own driver), then benchmark all four query types against
// the frozen artifacts and check the report.
func TestRunQueryColdFromArtifacts(t *testing.T) {
	chunkID := chunk.ID(0)
	packDir, txLedgers := writeSourcePack(t, t.TempDir(), chunkID, chunk.LedgersPerChunk)
	artifactRoot := t.TempDir()
	require.NoError(t, runCold(context.Background(), testLogger(), coldOptions{
		Source:       sourceConfig{Kind: sourcePack, PackDir: packDir},
		Types:        ingest.Config{Ledgers: true, Txhash: true, Events: true},
		StartChunk:   chunkID,
		NumChunks:    1,
		ChunkWorkers: 1,
		ArtifactRoot: artifactRoot,
		OutDir:       filepath.Join(t.TempDir(), "ingest-csv"),
	}))

	csvDir := filepath.Join(t.TempDir(), "csv")
	knobs := testQueryKnobs([]int{1, 2})
	knobs.OutDir = csvDir
	require.NoError(t, runQueryCold(context.Background(), testLogger(), coldQueryOptions{
		queryKnobs:        knobs,
		ColdRoot:          artifactRoot,
		StartChunk:        chunkID,
		NumChunks:         1,
		ReaderConcurrency: 2,
	}))

	// Every op is well above timer granularity (file opens, decompression),
	// so each cell's sample count is exactly workers × iters.
	ledgers := readCSV(t, filepath.Join(csvDir, "ledgers.csv"))
	require.Contains(t, ledgers, "total_c1")
	assert.EqualValues(t, 3, ledgers["total_c1"]["n"])
	assert.EqualValues(t, 3*knobs.LedgersPerRead, ledgers["total_c1"]["n_items"])
	require.Contains(t, ledgers, "total_c2")
	assert.EqualValues(t, 6, ledgers["total_c2"]["n"])

	txpage := readCSV(t, filepath.Join(csvDir, "txpage.csv"))
	assert.EqualValues(t, 3*knobs.PageSize, txpage["total_c1"]["n_items"])

	txhash := readCSV(t, filepath.Join(csvDir, "txhash.csv"))
	assert.EqualValues(t, 3, txhash["total_c1"]["n"])
	assert.EqualValues(t, 3, txhash["total_c1"]["n_items"])

	// Every drawn filter set constrains the fixture's single contract/topic,
	// so each query's post-filtered result is the chunk's full event count —
	// the strongest check that the REAL post-filtered path ran.
	events := readCSV(t, filepath.Join(csvDir, "events.csv"))
	assert.EqualValues(t, 3*txLedgers, events["total_c1"]["n_items"])
	assert.EqualValues(t, 6*txLedgers, events["total_c2"]["n_items"])

	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	for _, row := range []string{"ledgers_c1", "ledgers_c2", "txpage_c1", "txhash_c1", "events_c1"} {
		require.Contains(t, driver, row)
		assert.EqualValues(t, 1, driver[row]["n"], row)
	}
	assert.EqualValues(t, 3, driver["ledgers_c1"]["n_items"])
	assert.EqualValues(t, 6, driver["ledgers_c2"]["n_items"])
	// Untimed setup rows: the index build covered 1 chunk, the hash sample
	// pooled every fixture transaction, the corpus kept the fixture's one
	// contract + one topic term, and the preflight counted every transaction.
	assert.EqualValues(t, 1, driver["txhash_index_build"]["n_items"])
	assert.EqualValues(t, txLedgers, driver["txhash_corpus_sample"]["n_items"])
	assert.EqualValues(t, 2, driver["events_corpus_scan"]["n_items"])
	assert.EqualValues(t, txLedgers, driver["txpage_preflight"]["n_items"])
}

// TestRunQueryHotFromIngestedDB is the end-to-end hot path: populate a hot
// DB through the production hot ingest (bench-ingest's own driver), reopen
// it read-only, and benchmark all four query types against it.
func TestRunQueryHotFromIngestedDB(t *testing.T) {
	const numLedgers = 300
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, numLedgers)
	hotRoot := t.TempDir()
	require.NoError(t, runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		Chunk:      chunkID,
		NumLedgers: numLedgers,
		HotRoot:    hotRoot,
		OutDir:     filepath.Join(t.TempDir(), "ingest-csv"),
	}))

	csvDir := filepath.Join(t.TempDir(), "csv")
	knobs := testQueryKnobs([]int{2})
	knobs.Warmup = 1
	knobs.LedgersPerRead = 5
	knobs.PageSize = 2 // the 300-ledger fixture holds 3 transactions
	knobs.SampleLedgers = numLedgers
	knobs.OutDir = csvDir
	require.NoError(t, runQueryHot(context.Background(), testLogger(), hotQueryOptions{
		queryKnobs: knobs,
		HotRoot:    hotRoot,
		Chunk:      chunkID,
	}))

	txEventLedgers := numLedgers / eventEvery
	for _, tc := range []struct {
		file  string
		items int64
	}{
		{file: "ledgers.csv", items: 6 * 5},
		{file: "txpage.csv", items: 6 * 2},
		{file: "txhash.csv", items: 6},
		{file: "events.csv", items: 6 * int64(txEventLedgers)},
	} {
		rows := readCSV(t, filepath.Join(csvDir, tc.file))
		require.Contains(t, rows, "total_c2", tc.file)
		assert.EqualValues(t, 6, rows["total_c2"]["n"], tc.file)
		assert.Equal(t, tc.items, rows["total_c2"]["n_items"], tc.file)
	}

	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	assert.EqualValues(t, txEventLedgers, driver["txhash_corpus_sample"]["n_items"])
	assert.EqualValues(t, txEventLedgers, driver["txpage_preflight"]["n_items"])
}

// TestRunQueryHotRefusesMissingDB: pointing --hot-dir at a root with no
// ingested hot DB must fail up front with a pointer at bench-ingest.
func TestRunQueryHotRefusesMissingDB(t *testing.T) {
	knobs := testQueryKnobs([]int{1})
	knobs.OutDir = t.TempDir()
	err := runQueryHot(context.Background(), testLogger(), hotQueryOptions{
		queryKnobs: knobs,
		HotRoot:    t.TempDir(),
		Chunk:      chunk.ID(0),
	})
	require.ErrorContains(t, err, "bench-ingest hot")
}

// TestRunQueryColdRefusesMissingArtifacts: a cold root without the chunk's
// artifacts fails at inspection, before any sweep runs.
func TestRunQueryColdRefusesMissingArtifacts(t *testing.T) {
	knobs := testQueryKnobs([]int{1})
	knobs.OutDir = t.TempDir()
	err := runQueryCold(context.Background(), testLogger(), coldQueryOptions{
		queryKnobs: knobs,
		ColdRoot:   t.TempDir(),
		StartChunk: chunk.ID(0),
		NumChunks:  1,
	})
	require.Error(t, err)
}
