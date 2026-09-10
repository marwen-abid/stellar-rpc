package bench

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/hotchunk"
)

// TestCSVSinkExactOutput replays a fixed signal sequence from two goroutines
// and asserts the exact CSV bytes: aggregation is order-independent (sorted
// percentiles, summed totals), so the concurrent interleaving must not change
// the report. Covers both sink interfaces (ingest.MetricSink and the
// observability.Metrics signals RunBackfill emits), the zero-duration
// exclusion rule, row suppression, file suppression (no per-type txhash or
// events stage signals → no txhash.csv / events.csv), and the fixed row
// orders.
func TestCSVSinkExactOutput(t *testing.T) {
	sink := newCSVSink()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sink.ColdExtract(10, 1, nil)
		sink.ColdExtract(20, 2, nil)
		sink.ColdExtract(0, 5, nil) // zero duration: excluded, items dropped
		sink.IngestStage("ledgers", "write", 5, 1)
		sink.IngestStage("ledgers", "finalize", 7, 0)
		sink.ColdIngest("ledgers", 100, 10000, nil)
		sink.HotPhase(hotchunk.PhaseExtract, 10, 0, nil)
		sink.Freeze(300)
	}()
	go func() {
		defer wg.Done()
		sink.ColdExtract(30, 3, nil)
		sink.ColdExtract(40, 4, nil)
		sink.IngestStage("ledgers", "write", 5, 1)
		sink.ColdIngest("events", 50, 20, nil)
		sink.ColdChunkTotal(200)
		sink.Rebuild(40)
		sink.HotPhase(hotchunk.PhaseCommit, 50, 0, nil)
		sink.HotPhase(hotchunk.PhaseCommit, 60, 3, nil)
	}()
	wg.Wait()

	outDir := t.TempDir()
	written, err := sink.writeCSVs(outDir)
	require.NoError(t, err)
	require.Len(t, written, 3)

	want := map[string]string{
		"ledgers.csv": csvHeader + "\n" +
			"write,2,2,10,5,5,5,5\n" +
			"finalize,1,0,7,7,7,7,7\n",
		"hot.csv": csvHeader + "\n" +
			"extract,1,0,10,10,10,10,10\n" +
			"commit,2,3,110,60,60,60,60\n",
		"driver.csv": csvHeader + "\n" +
			"backfill_wall,1,0,300,300,300,300,300\n" +
			"index_rebuild,1,0,40,40,40,40,40\n" +
			"chunk_total,1,0,200,200,200,200,200\n" +
			"ledgers_total,1,10000,100,100,100,100,100\n" +
			"events_total,1,20,50,50,50,50,50\n" +
			"cold_extract,4,10,100,30,40,40,40\n",
	}
	for name, content := range want {
		got, rerr := os.ReadFile(filepath.Join(outDir, name))
		require.NoError(t, rerr, name)
		assert.Equal(t, content, string(got), name)
	}
	assert.NoFileExists(t, filepath.Join(outDir, "txhash.csv"))
	assert.NoFileExists(t, filepath.Join(outDir, "events.csv"))
}

// TestCSVSinkHotIngestTotal drives the ingest_total reconstruction directly:
// two complete per-ledger HotPhase bursts (extract→ledgers→txhash→events→
// commit→apply) plus one FAILED burst (extract, then a phase carrying an error,
// no apply). Only PhaseApply — the terminal, success-only phase — emits an
// ingest_total sample, so the failed burst contributes nothing; each complete
// burst contributes one sample (items=1) whose duration is the sum of that
// burst's phases.
func TestCSVSinkHotIngestTotal(t *testing.T) {
	sink := newCSVSink()

	// burst plays one complete per-ledger phase burst, each phase 1ns longer
	// than the last, so its six phases sum to base*6+21ns.
	burst := func(base time.Duration) {
		sink.HotPhase(hotchunk.PhaseExtract, base+1, 0, nil)
		sink.HotPhase(hotchunk.PhaseLedgers, base+2, 1, nil)
		sink.HotPhase(hotchunk.PhaseTxhash, base+3, 4, nil)
		sink.HotPhase(hotchunk.PhaseEvents, base+4, 2, nil)
		sink.HotPhase(hotchunk.PhaseCommit, base+5, 0, nil)
		sink.HotPhase(hotchunk.PhaseApply, base+6, 0, nil)
	}
	burst(100) // 6*100 + 21 = 621
	burst(200) // 6*200 + 21 = 1221

	// A failed ledger: extract ran, then PhaseCommit failed — no apply, so no
	// ingest_total sample.
	sink.HotPhase(hotchunk.PhaseExtract, 10, 0, nil)
	sink.HotPhase(hotchunk.PhaseCommit, 20, 0, errors.New("commit failed"))

	outDir := t.TempDir()
	_, err := sink.writeCSVs(outDir)
	require.NoError(t, err)

	driver := readCSV(t, filepath.Join(outDir, "driver.csv"))
	require.Contains(t, driver, "ingest_total")
	assert.EqualValues(t, 2, driver["ingest_total"]["n"])
	assert.EqualValues(t, 2, driver["ingest_total"]["n_items"])
	assert.EqualValues(t, 621+1221, driver["ingest_total"]["total_ns"])
}

// TestCSVSinkEmpty asserts a sink with no signals writes no files.
func TestCSVSinkEmpty(t *testing.T) {
	outDir := t.TempDir()
	written, err := newCSVSink().writeCSVs(outDir)
	require.NoError(t, err)
	assert.Empty(t, written)
	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestCSVSinkLastCommitted pins the gauge semantics the hot driver's
// completion check relies on: latest value wins, and dropped observability
// signals stay dropped.
func TestCSVSinkLastCommitted(t *testing.T) {
	sink := newCSVSink()
	assert.Zero(t, sink.lastCommittedSeq())
	sink.LastCommitted(41)
	sink.LastCommitted(42)
	assert.EqualValues(t, 42, sink.lastCommittedSeq())

	// No schedule attached, so LastCommitted records no pace_lag row.
	sink.RetentionFloor(7)
	sink.ChunkBoundary()
	sink.LiveHotChunks(3)
	sink.BackfillPass(10)
	sink.Discard(1, 10)
	sink.Prune(2, 10)
	written, err := sink.writeCSVs(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, written)
}

// TestCSVSinkPaceLag drives LastCommitted against an anchored schedule and an
// injected clock: each committed ledger's lag is now − its due time. Ledger 2
// (due at the anchor) commits 30ms in; ledger 3 stays on pace at 30ms; ledger 4
// falls 200ms behind. Assert the recorded per-ledger lag samples and that the
// aggregated pace_lag row carries one item per ledger.
func TestCSVSinkPaceLag(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	sched := &paceSchedule{interval: 100 * time.Millisecond, firstSeq: 2, clock: clock.now}
	sched.dueForPos(0) // anchor the schedule as the pacer would, at the first yield
	sink := newCSVSink()
	sink.schedule = sched

	clock.advance(30 * time.Millisecond) // ledger 2 (due t0) commits at t0+30
	sink.LastCommitted(2)
	clock.advance(100 * time.Millisecond) // ledger 3 (due t0+100) commits at t0+130
	sink.LastCommitted(3)
	clock.advance(270 * time.Millisecond) // ledger 4 (due t0+200) commits at t0+400
	sink.LastCommitted(4)

	assert.Equal(t, []time.Duration{30 * time.Millisecond, 30 * time.Millisecond, 200 * time.Millisecond},
		paceLagSamples(sink))

	driver := readCSV(t, filepath.Join(mustWriteCSVs(t, sink), "driver.csv"))
	require.Contains(t, driver, "pace_lag")
	assert.EqualValues(t, 3, driver["pace_lag"]["n"])
	assert.EqualValues(t, 3, driver["pace_lag"]["n_items"])
}

// TestCSVSinkPaceLagClampedToZero: a ledger committing before its due time (its
// lag would be negative) records a zero-lag sample. Zero-lag samples are part
// of the lag distribution (an on-time ledger), so a paced run whose only
// sample is on-time still writes a pace_lag row, with all percentiles 0.
func TestCSVSinkPaceLagClampedToZero(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	sched := &paceSchedule{interval: 100 * time.Millisecond, firstSeq: 2, clock: clock.now}
	sched.dueForPos(0)
	sink := newCSVSink()
	sink.schedule = sched

	sink.LastCommitted(5) // pos 3, due t0+300, clock still t0 → lag −300ms → clamped 0

	assert.Equal(t, []time.Duration{0}, paceLagSamples(sink)) // the raw (zero) sample is recorded
	driver := readCSV(t, filepath.Join(mustWriteCSVs(t, sink), "driver.csv"))
	require.Contains(t, driver, "pace_lag")
	assert.EqualValues(t, 1, driver["pace_lag"]["n"])
	assert.EqualValues(t, 1, driver["pace_lag"]["n_items"])
	assert.EqualValues(t, 0, driver["pace_lag"]["p50_ns"])
	assert.EqualValues(t, 0, driver["pace_lag"]["p99_ns"])
}

// TestCSVSinkPaceLagMixed: on-time (zero-lag) and late ledgers aggregate into
// one pace_lag row that counts every committed ledger, so the percentiles
// reflect how often the run kept pace, not just how late the late ledgers were.
func TestCSVSinkPaceLagMixed(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	sched := &paceSchedule{interval: 100 * time.Millisecond, firstSeq: 2, clock: clock.now}
	sched.dueForPos(0)
	sink := newCSVSink()
	sink.schedule = sched

	sink.LastCommitted(2) // due t0, commits at t0 → lag 0
	clock.advance(100 * time.Millisecond)
	sink.LastCommitted(3) // due t0+100, commits at t0+100 → lag 0
	clock.advance(200 * time.Millisecond)
	sink.LastCommitted(4) // due t0+200, commits at t0+300 → lag 100ms

	assert.Equal(t, []time.Duration{0, 0, 100 * time.Millisecond}, paceLagSamples(sink))

	driver := readCSV(t, filepath.Join(mustWriteCSVs(t, sink), "driver.csv"))
	require.Contains(t, driver, "pace_lag")
	assert.EqualValues(t, 3, driver["pace_lag"]["n"])
	assert.EqualValues(t, 3, driver["pace_lag"]["n_items"])
	assert.EqualValues(t, 0, driver["pace_lag"]["p50_ns"]) // 2 of 3 ledgers on time
	assert.Equal(t, (100 * time.Millisecond).Nanoseconds(), driver["pace_lag"]["max_ns"])
}

// TestCSVSinkPaceLagUnanchored: a commit arriving before the schedule anchors
// has no due time to score against, so it records nothing — guarding the
// pace_lag row against a garbage lag measured from the zero time.
func TestCSVSinkPaceLagUnanchored(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	sink := newCSVSink()
	sink.schedule = &paceSchedule{interval: 100 * time.Millisecond, firstSeq: 2, clock: clock.now}

	sink.LastCommitted(2)

	assert.Empty(t, paceLagSamples(sink))
	written, err := sink.writeCSVs(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, written)
}

// TestCSVSinkPaceLagBelowFirstSeq: a commit for a ledger below the schedule's
// firstSeq is off the schedule entirely, so it records nothing — guarding the
// pace_lag row against the wrapped uint32 offset a below-range seq would
// otherwise score against.
func TestCSVSinkPaceLagBelowFirstSeq(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	sched := &paceSchedule{interval: 100 * time.Millisecond, firstSeq: 2, clock: clock.now}
	sched.dueForPos(0)
	sink := newCSVSink()
	sink.schedule = sched

	sink.LastCommitted(1)

	assert.Empty(t, paceLagSamples(sink))
}

// TestCSVSinkKeepsZeroLagAndShedSamples: _lag and _shed rows with only zero
// samples are written; a latency row with only zero samples is dropped.
func TestCSVSinkKeepsZeroLagAndShedSamples(t *testing.T) {
	sink := newSchemaCSVSink(querySpecs([]string{queryTypeLedgers}, []float64{1}))
	sink.observe(fileDriver, "ledgers_r1_lag", 0, 1)
	sink.observe(fileDriver, "ledgers_r1_lag", 0, 1)
	sink.observe(fileDriver, "ledgers_r1_shed", 0, 0)
	sink.observe(queryTypeLedgers, "total_r1", 0, 1)

	outDir := t.TempDir()
	written, err := sink.writeCSVs(outDir)
	require.NoError(t, err)
	require.Len(t, written, 1)

	driver := readCSV(t, filepath.Join(outDir, "driver.csv"))
	require.Contains(t, driver, "ledgers_r1_lag")
	assert.EqualValues(t, 2, driver["ledgers_r1_lag"]["n"])
	require.Contains(t, driver, "ledgers_r1_shed")
	assert.EqualValues(t, 1, driver["ledgers_r1_shed"]["n"])
	assert.NoFileExists(t, filepath.Join(outDir, "ledgers.csv"))
}

func TestKeepsZeroSamples(t *testing.T) {
	for _, tc := range []struct {
		file, label string
		want        bool
	}{
		{fileDriver, "pace_lag", true},
		{fileDriver, "txhash_r300_lag", true},
		{fileDriver, "events_r0.5_shed", true},
		{fileDriver, "ledgers_r1_millirps", true},
		{fileQueryAccounting, "ledgers_r1_target_millirps", true},
		{fileQueryAccounting, "ledgers_r1_completion_millirps", true},
		{fileQueryAccounting, "ledgers_r1_scheduled", true},
		{fileQueryAccounting, "ledgers_r1_dispatched", true},
		{fileQueryAccounting, "ledgers_r1_successful", true},
		{fileQueryAccounting, "ledgers_r1_failed", true},
		{fileQueryAccounting, "ledgers_r1_shed", true},
		{fileQueryAccounting, "ledgers_r1_drain", true},
		{fileQueryAccounting, "ledgers_r1_arrival", false},
		{fileQueryAccounting, "ledgers_r1_elapsed", false},
		{fileDriver, "ledgers_r1", false},
		{fileDriver, "open", false},
		{queryTypeLedgers, "total_r1_lag", false},
		{fileHot, "commit_lag", false},
		{queryTypeLedgers, "total_r1_failed", false},
		{queryTypeLedgers, "total_r1_drain", false},
		{queryTypeLedgers, "total_r1_target_millirps", false},
		{queryTypeLedgers, "total_r1_completion_millirps", false},
	} {
		assert.Equal(t, tc.want, keepsZeroSamples(tc.file, tc.label), "keepsZeroSamples(%q, %q)", tc.file, tc.label)
	}
}

func TestLogQueryMetricRows(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		d      time.Duration
		items  int
		want   string
	}{
		{driverLegRPSSuffix, 1250, 0, "window_rps=1.250"},
		{driverLegTargetRPSSuffix, 2000, 0, "rps=2.000"},
		{driverLegCompletionRPSSuffix, 750, 0, "rps=0.750"},
		{driverLegScheduledSuffix, 0, 3, "scheduled=3"},
		{driverLegDispatchedSuffix, 0, 2, "dispatched=2"},
		{driverLegSuccessSuffix, 0, 1, "successful=1"},
		{driverLegFailedSuffix, 0, 1, "failed=1"},
		{driverLegShedSuffix, 0, 1, "shed=1"},
		{driverLegDrainSuffix, 0, 0, "total=0s"},
	} {
		t.Run(tc.suffix, func(t *testing.T) {
			var output bytes.Buffer
			logger := supportlog.New()
			logger.SetLevel(supportlog.InfoLevel)
			logger.SetOutput(&output)
			r := row{name: queryDriverLegRow(queryTypeLedgers, 2, tc.suffix), n: 1, total: tc.d, items: tc.items}
			for _, fileName := range []string{fileDriver, fileQueryAccounting} {
				output.Reset()
				logRow(logger, fileName, r)
				assert.Contains(t, output.String(), tc.want)
				if tc.suffix == driverLegTargetRPSSuffix || tc.suffix == driverLegCompletionRPSSuffix {
					assert.NotContains(t, output.String(), "window_rps")
				}
			}
			output.Reset()
			logRow(logger, queryTypeLedgers, r)
			assert.Contains(t, output.String(), "total=")
			assert.NotContains(t, output.String(), "rps=")
		})
	}
}

func TestLogRSSOnlyInDriver(t *testing.T) {
	for _, fileName := range []string{fileDriver, fileQueryAccounting, queryTypeLedgers} {
		t.Run(fileName, func(t *testing.T) {
			var output bytes.Buffer
			logger := supportlog.New()
			logger.SetLevel(supportlog.InfoLevel)
			logger.SetOutput(&output)
			logRow(logger, fileName, row{name: driverPeakRSS, n: 1, total: 1234})
			if fileName == fileDriver {
				assert.Contains(t, output.String(), "bytes=1234")
				assert.NotContains(t, output.String(), "total=")
			} else {
				assert.Contains(t, output.String(), "total=")
				assert.NotContains(t, output.String(), "bytes=")
			}
		})
	}
}

func TestQueryDriverSchema(t *testing.T) {
	specs := querySpecs([]string{queryTypeLedgers, queryTypeTxHash}, []float64{10, 0.5})
	wantRows := []string{
		"open", "evict",
		"ledgers_r10", "ledgers_r10_millirps", "ledgers_r10_lag", "ledgers_r10_shed",
		"ledgers_r0.5", "ledgers_r0.5_millirps", "ledgers_r0.5_lag", "ledgers_r0.5_shed",
		"txhash_r10", "txhash_r10_millirps", "txhash_r10_lag", "txhash_r10_shed",
		"txhash_r0.5", "txhash_r0.5_millirps", "txhash_r0.5_lag", "txhash_r0.5_shed",
		"peak_rss_bytes",
	}
	require.Len(t, specs, 4)
	assert.Equal(t, fileSpec{name: fileDriver, rowOrder: wantRows}, specs[2])
	assert.Equal(t, fileQueryAccounting, specs[3].name)
	assert.Equal(t, fileSpec{name: queryTypeLedgers, rowOrder: []string{
		"total_r10", "total_r0.5", "service_r10", "service_r0.5",
	}}, specs[0])
	assert.Equal(t, fileSpec{name: queryTypeTxHash, rowOrder: []string{
		"total_r10", "total_r0.5", "service_r10", "service_r0.5",
		"found_r10", "found_r0.5", "miss_r10", "miss_r0.5",
	}}, specs[1])
	sink := newSchemaCSVSink(specs)
	for i := len(wantRows) - 1; i >= 0; i-- {
		sink.observe(fileDriver, wantRows[i], time.Second, 1)
	}
	files := sink.files()
	require.Len(t, files, 1)
	names := make([]string, 0, len(files[0].rows))
	for _, r := range files[0].rows {
		names = append(names, r.name)
	}
	assert.Equal(t, wantRows, names)
}

func TestQueryAccountingSchema(t *testing.T) {
	specs := querySpecs([]string{queryTypeLedgers}, []float64{10})
	wantRows := []string{
		"ledgers_r10_target_millirps", "ledgers_r10_completion_millirps",
		"ledgers_r10_scheduled", "ledgers_r10_dispatched", "ledgers_r10_successful", "ledgers_r10_failed",
		"ledgers_r10_shed", "ledgers_r10_arrival", "ledgers_r10_elapsed", "ledgers_r10_drain",
	}
	require.Len(t, specs, 3)
	assert.Equal(t, fileSpec{name: fileQueryAccounting, rowOrder: wantRows}, specs[2])
	sink := newSchemaCSVSink(specs)
	// Record in reverse order to verify schema ordering as well as zero retention.
	for i := len(wantRows) - 1; i >= 0; i-- {
		name := wantRows[i]
		d, items := time.Duration(0), 0
		switch name {
		case "ledgers_r10_arrival", "ledgers_r10_elapsed":
			d = time.Second
		case "ledgers_r10_target_millirps":
			d = 10 * milliPerUnit
		case "ledgers_r10_scheduled", "ledgers_r10_dispatched", "ledgers_r10_failed":
			items = 10
		}
		sink.observe(fileQueryAccounting, name, d, items)
	}
	sink.observe(fileDriver, "ledgers_r10_millirps", 0, 0)
	sink.observe(fileDriver, "ledgers_r10_shed", 0, 0)
	files := sink.files()
	require.Len(t, files, 2)
	assert.Equal(t, fileDriver, files[0].name)
	assert.Equal(t, fileQueryAccounting, files[1].name)
	names := make([]string, 0, len(files[1].rows))
	for _, r := range files[1].rows {
		names = append(names, r.name)
	}
	assert.Equal(t, wantRows, names)
	outDir := mustWriteCSVs(t, sink)
	accounting := readCSV(t, filepath.Join(outDir, "query-accounting.csv"))
	for _, name := range names {
		assert.EqualValues(t, 1, accounting[name]["n"], name)
	}
	for _, name := range []string{"scheduled", "dispatched", "failed", "successful", "shed"} {
		r := accounting["ledgers_r10_"+name]
		want := 10
		if name == "successful" || name == "shed" {
			want = 0
		}
		assert.EqualValues(t, want, r["n_items"], name)
		assert.Zero(t, r["total_ns"], name)
	}
	assert.EqualValues(t, 10*milliPerUnit, accounting["ledgers_r10_target_millirps"]["total_ns"])
	assert.Zero(t, accounting["ledgers_r10_completion_millirps"]["total_ns"])
	assert.Zero(t, accounting["ledgers_r10_drain"]["total_ns"])
	driver := readCSV(t, filepath.Join(outDir, "driver.csv"))
	require.Len(t, driver, 2)
	assert.Zero(t, driver["ledgers_r10_millirps"]["total_ns"])
	assert.Equal(t, driver["ledgers_r10_shed"], accounting["ledgers_r10_shed"])
}

// mustWriteCSVs writes the sink's report to a temp dir and returns it.
func mustWriteCSVs(t *testing.T, sink *csvSink) string {
	t.Helper()
	outDir := t.TempDir()
	_, err := sink.writeCSVs(outDir)
	require.NoError(t, err)
	return outDir
}

// paceLagSamples returns the recorded pace_lag sample durations in record order.
func paceLagSamples(sink *csvSink) []time.Duration {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sr := sink.rows[rowKey{file: fileDriver, row: driverPaceLag}]
	if sr == nil {
		return nil
	}
	ds := make([]time.Duration, len(sr.samples))
	for i, sm := range sr.samples {
		ds[i] = sm.d
	}
	return ds
}
