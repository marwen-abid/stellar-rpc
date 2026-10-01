package bench

import (
	"context"
	"encoding/csv"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/adapters"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/catalog"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/rpcv2test"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/hotchunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/ledger"
)

// eventEvery: every eventEvery-th ledger of a fixture chunk carries one
// transaction with one contract event; the rest are zero-tx ledgers.
const eventEvery = 100

func testLogger() *supportlog.Entry {
	l := supportlog.New()
	l.SetLevel(logrus.ErrorLevel)
	return l
}

// writeSourcePack materializes a source ledger pack for chunkID under
// root/ledgers (the tree --pack-dir points at), containing numLedgers ledgers
// from the chunk's first sequence: every eventEvery-th one carries a
// transaction with one contract event, the rest are zero-tx. It returns the
// ledgers tree root and the number of tx/event-bearing ledgers written.
func writeSourcePack(t *testing.T, root string, chunkID chunk.ID, numLedgers uint32) (string, int) {
	t.Helper()
	layout := geometry.NewLayout(root)
	packPath := layout.LedgerPackPath(chunkID)
	require.NoError(t, os.MkdirAll(filepath.Dir(packPath), 0o755))

	w, err := ledger.NewColdWriter(packPath, chunkID.FirstLedger(), ledger.ColdWriterOptions{})
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	txLedgers := 0
	first := chunkID.FirstLedger()
	for seq := first; seq < first+numLedgers; seq++ {
		var raw []byte
		if (seq-first)%eventEvery == 0 {
			raw = rpcv2test.EventLCMBytes(t, seq)
			txLedgers++
		} else {
			raw = rpcv2test.ZeroTxLCMBytes(t, seq)
		}
		require.NoError(t, w.AppendLedger(seq, raw))
	}
	require.NoError(t, w.Commit())
	return layout.LedgersRoot(), txLedgers
}

// readCSV parses one report file into rows keyed by stage name; each row maps
// the header column name to its integer value.
func readCSV(t *testing.T, path string) map[string]map[string]int64 {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	records, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records)
	header := records[0]
	rows := make(map[string]map[string]int64, len(records)-1)
	for _, rec := range records[1:] {
		row := make(map[string]int64, len(header)-1)
		for i := 1; i < len(header); i++ {
			v, perr := strconv.ParseInt(rec[i], 10, 64)
			require.NoError(t, perr)
			row[header[i]] = v
		}
		rows[rec[0]] = row
	}
	return rows
}

// txhashIndexPath resolves where a backfill over [lo, hi] freezes its txhash
// index .idx (both chunks inside one window, as every test range here is).
func txhashIndexPath(t *testing.T, layout geometry.Layout, lo, hi chunk.ID) string {
	t.Helper()
	txLayout, err := geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)
	require.NoError(t, err)
	w := txLayout.TxHashIndexID(lo)
	require.Equal(t, w, txLayout.TxHashIndexID(hi), "test range must stay inside one index window")
	return layout.TxHashIndexFilePath(geometry.TxHashIndexCoverage{Index: w, Lo: lo, Hi: hi})
}

// openKeptCatalog opens the catalog a run left under root.
func openKeptCatalog(t *testing.T, root string) *catalog.Catalog {
	t.Helper()
	layout := geometry.NewLayout(root)
	txLayout, err := geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)
	require.NoError(t, err)
	cat, err := catalog.Open(layout.CatalogPath(), layout, txLayout, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

// readViewOn builds a registry over cat with every ready hot chunk published
// and latest as the tip, then asserts that a read view opens and that both
// close-time edges can be read.
func readViewOn(t *testing.T, cat *catalog.Catalog, latest uint32) {
	t.Helper()
	reg := query.NewRegistry(cat, rpcv2test.RetentionFor(t, cat, 0))
	ready, err := cat.ReadyHotChunkKeys()
	require.NoError(t, err)
	for _, c := range ready {
		db, err := hotchunk.OpenReadyView(geometry.HotReady, cat.Layout().HotChunkPath(c), c, testLogger())
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		reg.PublishHandle(c, db)
	}
	reg.SetLatestLedger(latest, query.UnknownCloseTime())
	view, err := reg.NewReadView()
	require.NoError(t, err)
	view.Release()
	require.NoError(t, adapters.SeedCloseTimes(reg))
}

// fileInfo is the part of a file's metadata that a write changes.
type fileInfo struct {
	size    int64
	modTime time.Time
}

// fileSet maps each path under root, relative to root, to its size and
// modification time.
func fileSet(t *testing.T, root string) map[string]fileInfo {
	t.Helper()
	files := map[string]fileInfo{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = fileInfo{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	require.NoError(t, err)
	return files
}

// TestRunColdFromPack is the end-to-end cold path: fabricate a full chunk's
// source pack, run the production backfill (RunBackfill: chunk freeze + txhash
// index build) through runCold, and check the CSV report and the cold
// artifacts.
func TestRunColdFromPack(t *testing.T) {
	chunkID := chunk.ID(0)
	packDir, txLedgers := writeSourcePack(t, t.TempDir(), chunkID, chunk.LedgersPerChunk)
	outRoot := t.TempDir()
	csvDir := filepath.Join(t.TempDir(), "csv")

	err := runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   outRoot,
		OutDir:     csvDir,
	})
	require.NoError(t, err)

	// Ledger writes are µs-scale (zstd), so every ledger lands in the write
	// row; finalize runs once per chunk.
	ledgers := readCSV(t, filepath.Join(csvDir, "ledgers.csv"))
	require.Contains(t, ledgers, "write")
	assert.EqualValues(t, chunk.LedgersPerChunk, ledgers["write"]["n"])
	assert.EqualValues(t, chunk.LedgersPerChunk, ledgers["write"]["n_items"])
	require.Contains(t, ledgers, "finalize")
	assert.EqualValues(t, 1, ledgers["finalize"]["n"])

	// Sub-tick (zero-duration) samples are excluded from n / n_items, so
	// per-ledger events rows only bound loosely; the finalize rows are
	// per-chunk and deterministic (txhash finalize items = total hashes).
	txhash := readCSV(t, filepath.Join(csvDir, "txhash.csv"))
	require.Contains(t, txhash, "finalize")
	assert.EqualValues(t, 1, txhash["finalize"]["n"])
	assert.EqualValues(t, txLedgers, txhash["finalize"]["n_items"])

	events := readCSV(t, filepath.Join(csvDir, "events.csv"))
	require.Contains(t, events, "write")
	require.Contains(t, events, "finalize")
	assert.EqualValues(t, 1, events["finalize"]["n"])

	// The driver rows: the engine's per-chunk aggregates (one sample each,
	// with the per-type ColdIngest item totals independent of timer
	// granularity) plus the scheduler's whole-run backfill wall and the one
	// index build the range plans.
	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	for _, name := range []string{
		"backfill_wall", "index_rebuild", "chunk_total", "ledgers_total", "txhash_total", "events_total",
	} {
		require.Contains(t, driver, name)
		assert.EqualValues(t, 1, driver[name]["n"], name)
	}
	assert.EqualValues(t, chunk.LedgersPerChunk, driver["ledgers_total"]["n_items"])
	assert.EqualValues(t, txLedgers, driver["txhash_total"]["n_items"])
	assert.EqualValues(t, txLedgers, driver["events_total"]["n_items"])

	// The shared per-ledger ExtractLedgerTxParts walk is ledger-scoped (no data
	// type), so it reports as its own driver row; per-ledger samples bound
	// loosely (sub-tick walks are excluded).
	require.Contains(t, driver, "cold_extract")

	// The rss_test.go tests use fake readers; this is the only assertion that
	// exercises the real readPeakRSS. It only works on Linux — which is what
	// CI runs — so on other platforms the read fails and the row is skipped.
	if runtime.GOOS == "linux" {
		require.Contains(t, driver, "peak_rss_bytes")
		assert.EqualValues(t, 1, driver["peak_rss_bytes"]["n"])
		assert.Positive(t, driver["peak_rss_bytes"]["total_ns"])
	}

	// The cold artifacts landed at the Layout-resolved paths — including the
	// cross-chunk txhash index the backfill builds beyond WriteColdChunk. The
	// window is partial (chunk 0 of a ChunksPerTxhashIndex-chunk window), so
	// the .bin inputs are NOT swept.
	layout := geometry.NewLayout(outRoot)
	assert.FileExists(t, layout.LedgerPackPath(chunkID))
	assert.FileExists(t, layout.TxHashBinPath(chunkID))
	for _, p := range layout.EventsPaths(chunkID) {
		assert.FileExists(t, p)
	}
	assert.FileExists(t, txhashIndexPath(t, layout, chunkID, chunkID))
}

// TestRunColdMultiChunk exercises the scheduler fan-out: two chunks backfilled
// with a two-slot worker pool against one shared sink, and one index build
// covering both.
func TestRunColdMultiChunk(t *testing.T) {
	srcRoot := t.TempDir()
	packDir, _ := writeSourcePack(t, srcRoot, chunk.ID(0), chunk.LedgersPerChunk)
	_, _ = writeSourcePack(t, srcRoot, chunk.ID(1), chunk.LedgersPerChunk)
	outRoot := t.TempDir()
	csvDir := filepath.Join(t.TempDir(), "csv")

	err := runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunk.ID(0),
		NumChunks:  2,
		Workers:    2,
		ColdRoot:   outRoot,
		OutDir:     csvDir,
	})
	require.NoError(t, err)

	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	assert.EqualValues(t, 2, driver["chunk_total"]["n"])
	assert.EqualValues(t, 2, driver["ledgers_total"]["n"])
	assert.Equal(t, 2*int64(chunk.LedgersPerChunk), driver["ledgers_total"]["n_items"])
	assert.EqualValues(t, 1, driver["backfill_wall"]["n"])
	assert.EqualValues(t, 1, driver["index_rebuild"]["n"])

	layout := geometry.NewLayout(outRoot)
	for c := chunk.ID(0); c <= chunk.ID(1); c++ {
		assert.FileExists(t, layout.LedgerPackPath(c))
		assert.FileExists(t, layout.TxHashBinPath(c))
	}
	assert.FileExists(t, txhashIndexPath(t, layout, chunk.ID(0), chunk.ID(1)))

	// The frontier hot chunk is end + 1, and it is the only ready hot chunk.
	cat := openKeptCatalog(t, outRoot)
	state, err := cat.HotState(chunk.ID(2))
	require.NoError(t, err)
	assert.Equal(t, geometry.HotReady, state)
	ready, err := cat.ReadyHotChunkKeys()
	require.NoError(t, err)
	assert.Equal(t, []chunk.ID{2}, ready)
}

// TestRunColdKeepsCatalog asserts that a cold run leaves a catalog with the
// earliest-ledger pin, frozen chunks and a ready frontier hot chunk, and that
// a read view opens over it.
func TestRunColdKeepsCatalog(t *testing.T) {
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, chunk.LedgersPerChunk)
	outRoot := t.TempDir()

	require.NoError(t, runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   outRoot,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}))

	cat := openKeptCatalog(t, outRoot)
	earliest, pinned, err := cat.EarliestLedger()
	require.NoError(t, err)
	require.True(t, pinned)
	assert.Equal(t, chunkID.FirstLedger(), earliest)
	state, err := cat.State(chunkID, geometry.KindLedgers)
	require.NoError(t, err)
	assert.Equal(t, geometry.StateFrozen, state)
	hotState, err := cat.HotState(chunkID + 1)
	require.NoError(t, err)
	assert.Equal(t, geometry.HotReady, hotState)
	assert.DirExists(t, geometry.NewLayout(outRoot).HotChunkPath(chunkID+1))

	readViewOn(t, cat, chunkID.LastLedger())
}

// TestRunColdRefusesExistingDataset asserts that a second cold run into the
// same root fails before it writes a file.
func TestRunColdRefusesExistingDataset(t *testing.T) {
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, chunk.LedgersPerChunk)
	outRoot := t.TempDir()
	opts := coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   outRoot,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}
	require.NoError(t, runCold(context.Background(), testLogger(), opts))
	before := fileSet(t, outRoot)

	opts.OutDir = filepath.Join(t.TempDir(), "csv2")
	err := runCold(context.Background(), testLogger(), opts)
	require.ErrorIs(t, err, errDatasetExists)
	assert.Equal(t, before, fileSet(t, outRoot))
	require.NoDirExists(t, opts.OutDir)
}

// TestRunColdRefusesInPlaceRepack asserts the source/destination collision
// guard.
func TestRunColdRefusesInPlaceRepack(t *testing.T) {
	root := t.TempDir()
	err := runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: geometry.NewLayout(root).LedgersRoot()},
		StartChunk: chunk.ID(0),
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   root,
		OutDir:     t.TempDir(),
	})
	require.ErrorContains(t, err, "must differ from --pack-dir")
}

// TestBenchRejectsInvalidSourceEarly asserts a bad --source invocation fails in
// validate(), before either driver creates its output or dataset directories.
func TestBenchRejectsInvalidSourceEarly(t *testing.T) {
	base := t.TempDir()
	coldRoot := filepath.Join(base, "cold")
	hotRoot := filepath.Join(base, "hot")
	outDir := filepath.Join(base, "csv")

	err := runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack}, // --pack-dir missing
		StartChunk: chunk.ID(0),
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   coldRoot,
		OutDir:     outDir,
	})
	require.ErrorContains(t, err, "--pack-dir is required")

	err = runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: "bogus"},
		StartChunk: chunk.ID(0),
		NumChunks:  1,
		HotRoot:    hotRoot,
		OutDir:     outDir,
	})
	require.ErrorContains(t, err, "expected pack|bsb")

	require.ErrorContains(t, sourceConfig{Kind: sourceBSB}.validate(), "--bucket-path is required")
	require.ErrorContains(t,
		sourceConfig{Kind: sourceBSB, BucketPath: "b", BufferBytes: -1}.validate(),
		"cannot be negative")

	for _, dir := range []string{coldRoot, hotRoot, outDir} {
		require.NoDirExists(t, dir, "invalid invocation must not create %s", dir)
	}
}

// TestPackBackendMultiChunkRange exercises the pack source's chunk routing: one
// bounded RawLedgers call spanning two packs streams both, in order — what the
// hot driver relies on when a run crosses a chunk boundary.
func TestPackBackendMultiChunkRange(t *testing.T) {
	srcRoot := t.TempDir()
	packDir, _ := writeSourcePack(t, srcRoot, chunk.ID(0), chunk.LedgersPerChunk)
	_, _ = writeSourcePack(t, srcRoot, chunk.ID(1), chunk.LedgersPerChunk)

	first := chunk.ID(0).LastLedger() - 2
	last := chunk.ID(1).FirstLedger() + 2
	next := first
	for raw, err := range (packBackend{root: packDir}).RawLedgers(
		context.Background(), ledgerbackend.BoundedRange(first, last),
	) {
		require.NoError(t, err)
		require.NotEmpty(t, raw)
		next++
	}
	require.Equal(t, last+1, next, "stream must cover the whole cross-chunk range")

	// A chunk with no pack fails fast with a clear error.
	for _, err := range (packBackend{root: packDir}).RawLedgers(
		context.Background(), ledgerbackend.BoundedRange(chunk.ID(2).FirstLedger(), chunk.ID(2).FirstLedger()),
	) {
		require.ErrorContains(t, err, "stat source pack")
	}
}

// TestRunHotFromPack is the end-to-end hot path: a capped run over a test
// dataset pack through the production ingestion loop into a fresh hot RocksDB,
// checking the per-phase report.
func TestRunHotFromPack(t *testing.T) {
	const numLedgers = 200
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, numLedgers)
	hotRoot := t.TempDir()
	csvDir := filepath.Join(t.TempDir(), "csv")

	require.NoError(t, runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		NumLedgers: numLedgers,
		HotRoot:    hotRoot,
		OutDir:     csvDir,
	}))

	// The commit phase (WAL append + fsync) is far above timer granularity,
	// so every ledger contributes a sample; extract likewise.
	hot := readCSV(t, filepath.Join(csvDir, "hot.csv"))
	require.Contains(t, hot, "extract")
	require.Contains(t, hot, "commit")
	assert.EqualValues(t, numLedgers, hot["commit"]["n"])
	require.Contains(t, hot, "apply")

	// The loop pulls the stream itself, so run_wall is the only row the driver
	// times directly; ingest_total is reconstructed inside the sink from each
	// ledger's HotPhase burst — one sample per ledger (items=1), giving the
	// per-ledger end-to-end latency the per-phase rows can't be summed into.
	// Every sample includes the fsync'd commit, so none is sub-tick: n counts
	// all numLedgers. (read_blocked was a bench-loop artifact and stays gone.)
	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	require.Contains(t, driver, "run_wall")
	assert.EqualValues(t, 1, driver["run_wall"]["n"])
	assert.EqualValues(t, numLedgers, driver["run_wall"]["n_items"])
	require.Contains(t, driver, "ingest_total")
	assert.EqualValues(t, numLedgers, driver["ingest_total"]["n"])
	assert.EqualValues(t, numLedgers, driver["ingest_total"]["n_items"])
	assert.NotContains(t, driver, "read_blocked")
	// An unpaced run must not add the pace_lag row — the CSV rows are a de
	// facto contract, and pace_lag belongs to --close-interval runs only.
	assert.NotContains(t, driver, "pace_lag")
	if runtime.GOOS == "linux" {
		require.Contains(t, driver, "peak_rss_bytes")
		assert.EqualValues(t, 1, driver["peak_rss_bytes"]["n"])
		assert.Positive(t, driver["peak_rss_bytes"]["total_ns"])
	}
}

// TestRunHotKeepsCatalog asserts that a hot run leaves a catalog with the
// earliest-ledger pin and a ready hot chunk, and that a read view opens over
// it.
func TestRunHotKeepsCatalog(t *testing.T) {
	const numLedgers = 200
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

	cat := openKeptCatalog(t, hotRoot)
	earliest, pinned, err := cat.EarliestLedger()
	require.NoError(t, err)
	require.True(t, pinned)
	first := chunkID.FirstLedger()
	assert.Equal(t, first, earliest)
	state, err := cat.HotState(chunkID)
	require.NoError(t, err)
	assert.Equal(t, geometry.HotReady, state)

	readViewOn(t, cat, first+numLedgers-1)
}

// TestRunHotRefusesExistingDataset asserts that a second hot run into the same
// root fails before it writes a file.
func TestRunHotRefusesExistingDataset(t *testing.T) {
	const numLedgers = 200
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, numLedgers)
	hotRoot := t.TempDir()
	opts := hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		NumLedgers: numLedgers,
		HotRoot:    hotRoot,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}
	require.NoError(t, runHot(context.Background(), testLogger(), opts))
	before := fileSet(t, hotRoot)

	opts.OutDir = filepath.Join(t.TempDir(), "csv2")
	err := runHot(context.Background(), testLogger(), opts)
	require.ErrorIs(t, err, errDatasetExists)
	assert.Equal(t, before, fileSet(t, hotRoot))
	require.NoDirExists(t, opts.OutDir)
}

// TestRunHotRefusesColdDataset asserts that a hot run into a cold dataset root
// fails.
func TestRunHotRefusesColdDataset(t *testing.T) {
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, chunk.LedgersPerChunk)
	root := t.TempDir()
	require.NoError(t, runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   root,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}))

	err := runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		NumLedgers: 200,
		HotRoot:    root,
		OutDir:     filepath.Join(t.TempDir(), "csv2"),
	})
	require.ErrorIs(t, err, errDatasetExists)
}

// TestRunHotIncompleteStream asserts an undersized source is a hard error, not
// a silently short report. A pack holding 50 ledgers covers seqs [2, 51]; asking
// for 60 makes runHot request [2, 61]. The pack stream requires the whole
// requested range to fall within its coverage, so it refuses the overshoot up
// front (stores.ErrOutOfRange, no ledgers streamed at all) and the loop surfaces
// that wrapped as "ingestion stream: ...". So it is that stream-error path that
// fires here, NOT runHot's own post-loop completion check (last-committed !=
// requested last): the loop returns the error before ingesting anything, so the
// completion check is never reached. Pin the exact refusal so an unrelated
// failure (pack-open error, config mistake) can't masquerade as this assertion.
func TestRunHotIncompleteStream(t *testing.T) {
	const packed = 50
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, packed)

	err := runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		NumLedgers: packed + 10, // asks for more than the pack holds
		HotRoot:    t.TempDir(),
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	})
	// Seqs are fixture-determined: 50 ledgers from chunk 0 → coverage [2, 51];
	// packed+10 requested → [2, 61]. Both bounds are deterministic, so pinning
	// them is exact rather than volatile.
	require.ErrorContains(t, err, "ingestion stream: stores: out of range: "+
		"requested [2, 61] outside store coverage [2, 51]")
}

// TestRunHotPaced is the end-to-end paced hot path: a capped run with a small
// --close-interval must hold each ledger to its due time, so the whole run
// cannot finish before the last ledger comes due — at least (n−1) intervals of
// real wall time — and the driver report must carry the pace rows the paced
// mode adds. The interval is chosen well above this machine's per-ledger
// ingest cost (a few ms of fsync'd commit), so unpaced ingestion alone cannot
// satisfy the bound — only real sleeping can. It is a lower bound, so the
// timing assertion never flakes.
func TestRunHotPaced(t *testing.T) {
	const numLedgers = 20
	const interval = 25 * time.Millisecond
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, numLedgers)
	csvDir := filepath.Join(t.TempDir(), "csv")

	start := time.Now()
	require.NoError(t, runHot(context.Background(), testLogger(), hotOptions{
		Source:        sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk:    chunkID,
		NumChunks:     1,
		NumLedgers:    numLedgers,
		HotRoot:       t.TempDir(),
		CloseInterval: interval,
		OutDir:        csvDir,
	}))
	// The last ledger (position numLedgers-1) yields no sooner than its due time,
	// anchor + (numLedgers-1)*interval, and the anchor is set after this start.
	assert.GreaterOrEqual(t, time.Since(start), time.Duration(numLedgers-1)*interval)

	// The paced run adds the pace_lag row: every ledger's ingest takes real
	// time, so no lag sample is zero and the row is not suppressed.
	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	require.Contains(t, driver, "pace_lag")
	assert.EqualValues(t, numLedgers, driver["pace_lag"]["n_items"])
}
