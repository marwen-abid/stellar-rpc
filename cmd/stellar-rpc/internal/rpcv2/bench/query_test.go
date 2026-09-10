package bench

import (
	"context"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/geometry"
)

// TestNewQueryCommand builds the full command tree, which executes every
// markRequired call, and checks each subcommand's required flags.
func TestNewQueryCommand(t *testing.T) {
	cmd := NewQueryCommand()
	require.Equal(t, "bench-query", cmd.Use)
	assertRequiredFlags(t, cmd, map[string][]string{
		"cold": {"start-chunk", "cold-dir"},
		"hot":  {"chunk", "hot-dir"},
	})
}

// TestQueryCommandAcceptsRunnerArgv parses the argv the campaign runner emits
// through both subcommands. The runner is stellar-experimental/stellar-rpc-benchmarks
// (runner/internal/plan/plan.go); it passes --warmup to hot only.
func TestQueryCommandAcceptsRunnerArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{
			name: "cold",
			argv: []string{
				"cold",
				"--cold-dir=/bench/ds", "--start-chunk=7", "--num-chunks=1",
				"--types=ledgers,txpage,txhash,events",
				"--target-rps=0.5,1,2", "--duration=5s", "--out=/bench/out",
			},
		},
		{
			name: "hot uncapped",
			argv: []string{
				"hot",
				"--hot-dir=/bench/hot", "--chunk=7",
				"--types=ledgers,txpage,txhash,events",
				"--target-rps=0.5,1,2", "--duration=5s", "--warmup=20", "--out=/bench/out",
			},
		},
		{
			name: "hot capped",
			argv: []string{
				"hot",
				"--hot-dir=/bench/hot", "--chunk=7",
				"--types=ledgers,txpage,txhash,events",
				"--target-rps=0.5,1,2", "--duration=5s", "--warmup=20",
				"--sample-ledgers=50000", "--out=/bench/out",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := subcommandsByUse(NewQueryCommand())[tc.argv[0]]
			require.NotNil(t, sub)
			require.NoError(t, sub.ParseFlags(tc.argv[1:]))

			types, err := sub.Flags().GetString("types")
			require.NoError(t, err)
			parsed, err := parseQueryTypes(types)
			require.NoError(t, err)
			require.Equal(t, allQueryTypes, parsed, "the runner runs every query type")

			targetRPS, err := sub.Flags().GetString("target-rps")
			require.NoError(t, err)
			rates, err := parseTargetRPS(targetRPS)
			require.NoError(t, err)
			require.Equal(t, []float64{0.5, 1, 2}, rates)

			duration, err := sub.Flags().GetDuration("duration")
			require.NoError(t, err)
			require.Equal(t, 5*time.Second, duration)

			warmup, err := sub.Flags().GetInt("warmup")
			require.NoError(t, err)
			if tc.argv[0] == "cold" {
				assert.Zero(t, warmup)
			} else {
				assert.Equal(t, 20, warmup)
			}
		})
	}
}

// TestQueryDefaultShapesMatchSLA checks the default read shapes against the
// team SLA. The campaign runner passes neither flag.
func TestQueryDefaultShapesMatchSLA(t *testing.T) {
	for _, name := range []string{"cold", "hot"} {
		t.Run(name, func(t *testing.T) {
			sub := subcommandsByUse(NewQueryCommand())[name]
			require.NotNil(t, sub)

			ledgersSpan, err := sub.Flags().GetUint32("ledgers-span")
			require.NoError(t, err)
			require.Equal(t, uint32(10), ledgersSpan, "SLA: getLedgers max=10")

			eventsLimit, err := sub.Flags().GetInt("events-limit")
			require.NoError(t, err)
			require.Equal(t, 10, eventsLimit, "SLA: getEvents small page, 10 matches")

			warmup, err := sub.Flags().GetInt("warmup")
			require.NoError(t, err)
			if name == "cold" {
				assert.Zero(t, warmup)
			} else {
				assert.Equal(t, 20, warmup)
			}
			corpusSize, err := sub.Flags().GetInt("txhash-corpus-size")
			require.NoError(t, err)
			assert.Equal(t, 512, corpusSize)
		})
	}
}

func TestParseQueryTypes(t *testing.T) {
	got, err := parseQueryTypes("events,ledgers")
	require.NoError(t, err)
	require.Equal(t, []string{"events", "ledgers"}, got, "the caller's order is kept")

	for _, bad := range []string{"", "ledgers,", "ledgers,ledgers", "ledger", "ledgers,txpages"} {
		_, err := parseQueryTypes(bad)
		require.Error(t, err, "--types=%q must be rejected", bad)
	}
}

func TestParseTargetRPS(t *testing.T) {
	got, err := parseTargetRPS("2,0.5,1")
	require.NoError(t, err)
	require.Equal(t, []float64{2, 0.5, 1}, got, "the caller's order is kept")

	for _, bad := range []string{"", "1,", "0", "-1", "1,1", "four", "NaN", "Inf", "1e7"} {
		_, err := parseTargetRPS(bad)
		require.Error(t, err, "--target-rps=%q must be rejected", bad)
	}
}

func TestFormatRPS(t *testing.T) {
	for rps, want := range map[float64]string{0.5: "0.5", 1: "1", 1.67: "1.67", 300: "300"} {
		assert.Equal(t, want, formatRPS(rps))
	}
	assert.Equal(t, "total_r0.5", queryTotalRow(0.5))
	assert.Equal(t, "txhash_r300_lag", queryDriverLegRow(queryTypeTxHash, 300, driverLegLagSuffix))
}

// TestQuerySpecs pins the report schema the results converter parses.
func TestQuerySpecs(t *testing.T) {
	specs := querySpecs([]string{queryTypeLedgers, queryTypeEvents}, []float64{0.5, 2})

	names := make([]string, len(specs))
	byName := make(map[string][]string, len(specs))
	for i, s := range specs {
		names[i] = s.name
		byName[s.name] = s.rowOrder
	}
	require.Equal(t, []string{"ledgers", "events", "driver", "query-accounting"}, names,
		"the query types come first, in --types order, then driver.csv and query-accounting.csv")
	require.Equal(t, []string{"total_r0.5", "total_r2", "service_r0.5", "service_r2"}, byName["ledgers"])
	require.Equal(t, []string{"total_r0.5", "total_r2", "service_r0.5", "service_r2"}, byName["events"])
	wantDriver := append([]string{"open", "evict"}, expectedQueryDriverRows(
		"ledgers_r0.5", "ledgers_r2", "events_r0.5", "events_r2",
	)...)
	wantDriver = append(wantDriver, "peak_rss_bytes")
	require.Equal(t, wantDriver, byName["driver"])
	require.Equal(t, expectedQueryAccountingRows(
		"ledgers_r0.5", "ledgers_r2", "events_r0.5", "events_r2",
	), byName[fileQueryAccounting])
}

func expectedQueryDriverRows(legs ...string) []string {
	rows := make([]string, 0, 4*len(legs))
	for _, leg := range legs {
		for _, suffix := range []string{"", "_millirps", "_lag", "_shed"} {
			rows = append(rows, leg+suffix)
		}
	}
	return rows
}

func expectedQueryAccountingRows(legs ...string) []string {
	rows := make([]string, 0, 10*len(legs))
	for _, leg := range legs {
		for _, suffix := range []string{
			"_target_millirps", "_completion_millirps",
			"_scheduled", "_dispatched", "_successful", "_failed", "_shed", "_arrival", "_elapsed", "_drain",
		} {
			rows = append(rows, leg+suffix)
		}
	}
	return rows
}

func TestQuerySpecsTxHashStages(t *testing.T) {
	specs := querySpecs([]string{queryTypeTxHash}, []float64{0.5, 2})
	require.Equal(t, queryTypeTxHash, specs[0].name)
	require.Equal(t, []string{
		"total_r0.5", "total_r2",
		"service_r0.5", "service_r2",
		"found_r0.5", "found_r2",
		"miss_r0.5", "miss_r2",
	}, specs[0].rowOrder)
}

// TestQuerySinkWritesContractRows checks that the _lag and _shed rows keep
// zero-duration samples and that _millirps is the answered requests over the
// offered window, times 1000.
func TestQuerySinkWritesContractRows(t *testing.T) {
	const (
		legWall    = 2 * time.Second
		legOffered = 4 * time.Second
	)
	types := []string{queryTypeLedgers, queryTypeTxHash}
	rates := []float64{1, 4}
	sink := newSchemaCSVSink(querySpecs(types, rates))
	sink.observe(fileDriver, driverQueryOpen, time.Millisecond, 1)
	res := legResult{
		samples: []cellSample{
			{service: time.Millisecond, scheduled: 2 * time.Millisecond, items: 3},
			{service: 2 * time.Millisecond, scheduled: 3 * time.Millisecond, items: 3},
		},
		lags:       []time.Duration{0, time.Millisecond},
		offered:    legOffered,
		wall:       legWall,
		elapsed:    legOffered,
		drain:      0,
		scheduled:  2,
		dispatched: 2,
	}
	for _, qtype := range types {
		for _, rps := range rates {
			recordLeg(sink, qtype, rps, res)
		}
	}

	outDir := t.TempDir()
	written, err := sink.writeCSVs(outDir)
	require.NoError(t, err)
	require.Len(t, written, 4)

	driver := readCSV(t, filepath.Join(outDir, "driver.csv"))
	accounting := readCSV(t, filepath.Join(outDir, fileQueryAccounting+".csv"))
	wantMilliRPS := int64(math.Round(float64(len(res.samples)) / legOffered.Seconds() * 1000))
	for _, qtype := range types {
		rows := readCSV(t, filepath.Join(outDir, qtype+".csv"))
		for _, rps := range rates {
			assert.Contains(t, rows, queryTotalRow(rps))
			assert.Contains(t, rows, queryServiceRow(rps))

			lag := queryDriverLegRow(qtype, rps, driverLegLagSuffix)
			require.Contains(t, driver, lag, "a zero dispatch lag is a real observation")
			assert.EqualValues(t, 2, driver[lag]["n"], "%s keeps its zero-lag sample", lag)

			shed := queryDriverLegRow(qtype, rps, driverLegShedSuffix)
			require.Contains(t, driver, shed, "a leg that shed nothing still reports a shed row")
			assert.EqualValues(t, 0, driver[shed]["n_items"])

			millirps := queryDriverLegRow(qtype, rps, driverLegRPSSuffix)
			require.Contains(t, driver, millirps)
			assert.Equal(t, wantMilliRPS, driver[millirps]["total_ns"])
			assert.EqualValues(t, len(res.samples), driver[millirps]["n_items"])
			assertLegDriverRows(t, driver, accounting, qtype, rps, int64(res.scheduled))
		}
	}

	// Rows that recorded nothing (evict, peak_rss_bytes) are absent from the file.
	for _, f := range sink.files() {
		if f.name != fileDriver && f.name != fileQueryAccounting {
			continue
		}
		names := make([]string, len(f.rows))
		for i, r := range f.rows {
			names[i] = r.name
		}
		want := append([]string{"open"}, expectedQueryDriverRows(
			"ledgers_r1", "ledgers_r4", "txhash_r1", "txhash_r4",
		)...)
		if f.name == fileQueryAccounting {
			want = expectedQueryAccountingRows("ledgers_r1", "ledgers_r4", "txhash_r1", "txhash_r4")
		}
		require.Equal(t, want, names)
	}
}

func TestRecordLegAllShed(t *testing.T) {
	const (
		rps       = 4.0
		shedLag   = 10 * time.Millisecond
		shedCount = 5
	)
	sink := newSchemaCSVSink(querySpecs([]string{queryTypeLedgers}, []float64{rps}))
	lags := make([]time.Duration, shedCount)
	for i := range lags {
		lags[i] = shedLag
	}
	recordLeg(sink, queryTypeLedgers, rps, legResult{
		lags: lags, scheduled: shedCount, shed: shedCount,
		offered: 1250 * time.Millisecond, elapsed: 1250 * time.Millisecond, wall: 0, drain: 0,
	})

	outDir := t.TempDir()
	written, err := sink.writeCSVs(outDir)
	require.NoError(t, err)
	require.Len(t, written, 2, "driver.csv and query-accounting.csv are written")

	driver := readCSV(t, filepath.Join(outDir, "driver.csv"))
	accounting := readCSV(t, filepath.Join(outDir, fileQueryAccounting+".csv"))
	shed := queryDriverLegRow(queryTypeLedgers, rps, driverLegShedSuffix)
	require.Contains(t, driver, shed)
	assert.EqualValues(t, shedCount, driver[shed]["n_items"])
	require.Contains(t, accounting, shed)
	assert.Equal(t, driver[shed], accounting[shed])

	millirps := queryDriverLegRow(queryTypeLedgers, rps, driverLegRPSSuffix)
	require.Contains(t, driver, millirps, "a leg that answered nothing still reports its rate")
	assert.EqualValues(t, 0, driver[millirps]["total_ns"])

	lag := queryDriverLegRow(queryTypeLedgers, rps, driverLegLagSuffix)
	require.Contains(t, driver, lag)
	assert.EqualValues(t, shedCount, driver[lag]["n"], "a shed position is charged a lag")
	for suffix, want := range map[string]int64{
		"_scheduled": shedCount, "_dispatched": 0, "_successful": 0, "_failed": 0,
	} {
		name := queryDriverLegRow(queryTypeLedgers, rps, suffix)
		require.Contains(t, accounting, name)
		assert.Equal(t, want, accounting[name]["n_items"])
	}
	for _, suffix := range []string{"_completion_millirps", "_drain"} {
		name := queryDriverLegRow(queryTypeLedgers, rps, suffix)
		require.Contains(t, accounting, name)
		assert.Zero(t, accounting[name]["total_ns"])
		assert.Zero(t, accounting[name]["n_items"])
	}

	assert.NoFileExists(t, filepath.Join(outDir, queryTypeLedgers+".csv"))
}

// TestOpenColdFixtureServesFrozenChunks opens the cold fixture over a tree
// bench-ingest produced. bench-ingest discards its catalog; the fixture
// rebuilds it.
func TestOpenColdFixtureServesFrozenChunks(t *testing.T) {
	chunkID := chunk.ID(0)
	coldRoot := ingestColdChunk(t, chunkID)

	f, release, err := openColdFixture(testLogger(), coldQueryOptions{
		ColdRoot:   coldRoot,
		StartChunk: chunkID,
		NumChunks:  1,
	})
	require.NoError(t, err)
	defer release()

	require.Equal(t, []chunk.ID{chunkID}, f.Chunks)
	assert.Equal(t, chunkID.FirstLedger(), f.FirstLedger)
	assert.Equal(t, chunkID.LastLedger(), f.LastLedger)

	view, err := f.view()
	require.NoError(t, err)
	defer view.Release()

	from := chunkID.FirstLedger()
	scan, err := view.ScanLedgers(from, from+9)
	require.NoError(t, err)
	next := from
	for e, serr := range scan {
		require.NoError(t, serr)
		assert.Equal(t, next, e.Seq)
		next++
	}
	assert.Equal(t, from+10, next, "the scan yielded every ledger in the range")

	// The view opens the .idx on the first probe; a wrong window coverage fails
	// there.
	idxs, err := view.ColdTxIndexes()
	require.NoError(t, err)
	require.Len(t, idxs, 1)

	s := newTxHashSampler(testRNG())
	require.NoError(t, s.sampleChunk(view, chunkID, f.FirstLedger, f.LastLedger))
	hashes := s.hashes
	require.NotEmpty(t, hashes, "the synthetic chunk holds transactions to probe")
	seq, err := idxs[0].Get(hashes[0])
	require.NoError(t, err)
	assert.GreaterOrEqual(t, seq, f.FirstLedger)
	assert.LessOrEqual(t, seq, f.LastLedger)
}

func testRNG() *rand.Rand { return rand.New(rand.NewPCG(defaultSeed, defaultSeed)) }

// testQueryPlan is the plan the end-to-end tests run: every type at two rates,
// 10 requests per leg at 50 rps and 40 at 200 rps.
func testQueryPlan() queryPlan {
	return queryPlan{
		Types:        allQueryTypes,
		TargetRPS:    []float64{50, 200},
		Duration:     200 * time.Millisecond,
		LedgersSpan:  4,
		TxPageSpan:   2,
		TxPageLimit:  10,
		EventsLimit:  10,
		MissFraction: 0.5,
		Passphrase:   network.PublicNetworkPassphrase,
		Seed:         defaultSeed,
	}
}

func TestRunQueryCold(t *testing.T) {
	chunkID := chunk.ID(0)
	coldRoot := ingestColdChunk(t, chunkID)

	csvDir := filepath.Join(t.TempDir(), "csv")
	plan := testQueryPlan()
	plan.Evict = true
	require.NoError(t, runQueryCold(context.Background(), testLogger(), coldQueryOptions{
		ColdRoot:   coldRoot,
		StartChunk: chunkID,
		NumChunks:  1,
		Plan:       plan,
		OutDir:     csvDir,
	}))

	assertQueryReport(t, csvDir, plan)

	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	require.Contains(t, driver, "open")
	assert.EqualValues(t, 1, driver["open"]["n_items"], "one chunk opened")
	if !evictSupported {
		// The pass is a no-op here; the sink drops zero-duration samples, so the
		// row may be absent.
		return
	}
	require.Contains(t, driver, "evict", "a cold leg evicts before it measures")
	assert.EqualValues(t, len(plan.Types)*len(plan.TargetRPS), driver["evict"]["n"],
		"one eviction pass per leg")
	assert.Positive(t, driver["evict"]["n_items"], "the eviction pass named some artifacts")
}

func TestRunQueryHot(t *testing.T) {
	const ingested = 2 * eventEvery
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, ingested)
	hotRoot := ingestHotChunk(t, packDir, ingested)

	csvDir := filepath.Join(t.TempDir(), "csv")
	plan := testQueryPlan()
	plan.Warmup = 2
	require.NoError(t, runQueryHot(context.Background(), testLogger(), hotQueryOptions{
		HotRoot: hotRoot,
		Chunk:   chunkID,
		Plan:    plan,
		OutDir:  csvDir,
	}))

	assertQueryReport(t, csvDir, plan)

	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	assert.NotContains(t, driver, "evict", "a hot run has no page-cache artifacts to evict")
}

// assertQueryReport checks a run's CSVs: every leg present with one sample per
// scheduled request, its driver rows written, and txhash's found and miss rows
// partitioning the total row.
func assertQueryReport(t *testing.T, csvDir string, plan queryPlan) {
	t.Helper()
	driver := readCSV(t, filepath.Join(csvDir, "driver.csv"))
	accounting := readCSV(t, filepath.Join(csvDir, fileQueryAccounting+".csv"))

	for _, qtype := range plan.Types {
		rows := readCSV(t, filepath.Join(csvDir, qtype+".csv"))
		for _, rps := range plan.TargetRPS {
			measured := int64(measuredRequests(rps, plan.Duration))
			total := queryTotalRow(rps)
			require.Contains(t, rows, total, "%s is missing its %s leg", qtype, formatRPS(rps))
			assert.Equal(t, measured, rows[total]["n"],
				"%s at %s rps must hold one sample per request", qtype, formatRPS(rps))

			service := queryServiceRow(rps)
			require.Contains(t, rows, service, "%s is missing %s", qtype, service)
			assert.Equal(t, measured, rows[service]["n"])

			assertLegDriverRows(t, driver, accounting, qtype, rps, measured)
			base := queryDriverRow(qtype, rps)
			assert.Equal(t, measured, accounting[base+"_successful"]["n_items"])
			interval := time.Duration(float64(time.Second) / rps)
			assert.Equal(t, measured*int64(interval), accounting[base+"_arrival"]["total_ns"])
		}
		if qtype != queryTypeTxHash {
			continue
		}
		for _, rps := range plan.TargetRPS {
			found := rows[queryStageRow(txHashStageFound, rps)]
			miss := rows[queryStageRow(txHashStageMiss, rps)]
			assert.Equal(t, rows[queryTotalRow(rps)]["n"], found["n"]+miss["n"],
				"txhash at %s rps: the found and miss rows must partition the blended row", formatRPS(rps))
		}
	}
}

// assertLegDriverRows checks measured counts, accounting windows and rate units.
func assertLegDriverRows(
	t *testing.T, driver, accounting map[string]map[string]int64, qtype string, rps float64, measured int64,
) {
	t.Helper()
	base := queryDriverRow(qtype, rps)
	for _, name := range expectedQueryDriverRows(base) {
		require.Contains(t, driver, name)
		if name != base+"_lag" {
			assert.EqualValues(t, 1, driver[name]["n"], name)
		}
	}
	for _, name := range expectedQueryAccountingRows(base) {
		require.Contains(t, accounting, name)
		assert.EqualValues(t, 1, accounting[name]["n"], name)
		if name != base+"_shed" {
			assert.NotContains(t, driver, name, "accounting must not change the legacy driver schema")
		}
	}
	assert.Equal(t, driver[base+"_shed"], accounting[base+"_shed"])
	scheduled := accounting[base+"_scheduled"]["n_items"]
	dispatched := accounting[base+"_dispatched"]["n_items"]
	successful := accounting[base+"_successful"]["n_items"]
	assert.Equal(t, measured, scheduled)
	assert.Equal(t, scheduled, dispatched+driver[base+"_shed"]["n_items"])
	assert.Equal(t, dispatched, successful+accounting[base+"_failed"]["n_items"])
	assert.Equal(t, scheduled, driver[base+"_lag"]["n"])
	assert.Equal(t, successful, driver[base]["n_items"])
	assert.Equal(t, successful, driver[base+"_millirps"]["n_items"])
	for _, suffix := range []string{"_scheduled", "_dispatched", "_successful", "_failed", "_shed"} {
		assert.Zero(t, accounting[base+suffix]["total_ns"], suffix)
	}
	wall := driver[base]["total_ns"]
	arrival := accounting[base+"_arrival"]["total_ns"]
	elapsed := accounting[base+"_elapsed"]["total_ns"]
	require.Positive(t, arrival)
	require.Positive(t, elapsed)
	assert.Equal(t, max(arrival, wall), elapsed)
	assert.Equal(t, max(wall-arrival, 0), accounting[base+"_drain"]["total_ns"])
	for _, suffix := range []string{
		"_arrival", "_elapsed", "_drain", "_target_millirps", "_completion_millirps",
	} {
		assert.Zero(t, accounting[base+suffix]["n_items"], suffix)
	}
	for suffix, want := range map[string]int64{
		"_target_millirps":     int64(math.Round(rps * 1000)),
		"_millirps":            int64(math.Round(float64(successful) / time.Duration(arrival).Seconds() * 1000)),
		"_completion_millirps": int64(math.Round(float64(successful) / time.Duration(elapsed).Seconds() * 1000)),
	} {
		rows := accounting
		if suffix == "_millirps" {
			rows = driver
		}
		for _, column := range []string{"total_ns", "p50_ns", "p90_ns", "p99_ns", "max_ns"} {
			assert.Equal(t, want, rows[base+suffix][column], "%s %s", suffix, column)
		}
	}
}

// ingestColdChunk runs bench-ingest cold over a synthetic pack and returns the
// frozen artifact root.
func ingestColdChunk(t *testing.T, chunkID chunk.ID) string {
	t.Helper()
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, chunk.LedgersPerChunk)
	coldRoot := t.TempDir()
	require.NoError(t, runCold(context.Background(), testLogger(), coldOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunkID,
		NumChunks:  1,
		Workers:    1,
		ColdRoot:   coldRoot,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}))
	return coldRoot
}

// ingestHotChunk runs bench-ingest hot over numLedgers ledgers of chunk 0 from
// packDir and returns the hot database root.
func ingestHotChunk(t *testing.T, packDir string, numLedgers uint32) string {
	t.Helper()
	hotRoot := t.TempDir()
	require.NoError(t, runHot(context.Background(), testLogger(), hotOptions{
		Source:     sourceConfig{Kind: sourcePack, PackDir: packDir},
		StartChunk: chunk.ID(0),
		NumChunks:  1,
		NumLedgers: numLedgers,
		HotRoot:    hotRoot,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	}))
	return hotRoot
}

func TestQueryRejectsWrongPassphrase(t *testing.T) {
	chunkID := chunk.ID(0)
	coldRoot := ingestColdChunk(t, chunkID)

	plan := testQueryPlan()
	plan.Types = []string{queryTypeTxHash}
	plan.Passphrase = network.TestNetworkPassphrase
	err := runQueryCold(context.Background(), testLogger(), coldQueryOptions{
		ColdRoot:   coldRoot,
		StartChunk: chunkID,
		NumChunks:  1,
		Plan:       plan,
		OutDir:     filepath.Join(t.TempDir(), "csv"),
	})
	require.ErrorContains(t, err, "--network-passphrase")
}

func TestOpenColdFixtureWithoutTxHashIndex(t *testing.T) {
	chunkID := chunk.ID(0)
	coldRoot := ingestColdChunk(t, chunkID)
	indexRoot := geometry.NewLayout(coldRoot).TxHashIndexRoot()
	require.DirExists(t, indexRoot, "the cold ingest built an index to remove")
	require.NoError(t, os.RemoveAll(indexRoot))

	t.Run("txhash requested", func(t *testing.T) {
		_, _, err := openColdFixture(testLogger(), coldQueryOptions{
			ColdRoot:   coldRoot,
			StartChunk: chunkID,
			NumChunks:  1,
			Plan:       queryPlan{Types: []string{queryTypeLedgers, queryTypeTxHash}},
		})
		require.ErrorContains(t, err, indexRoot, "the error names where the index is expected")
		assert.ErrorContains(t, err, queryTypeTxHash)
	})

	t.Run("txhash not requested", func(t *testing.T) {
		logger := testLogger()
		done := logger.StartTest(logrus.WarnLevel)
		f, release, err := openColdFixture(logger, coldQueryOptions{
			ColdRoot:   coldRoot,
			StartChunk: chunkID,
			NumChunks:  1,
			Plan:       queryPlan{Types: []string{queryTypeLedgers}},
		})
		warnings := done()
		require.NoError(t, err)
		defer release()
		assert.Equal(t, []chunk.ID{chunkID}, f.Chunks)
		assert.Contains(t, strings.Join(logMessages(warnings, logrus.WarnLevel), "\n"),
			"no tx-hash window index on disk")
	})
}

// logMessages returns the messages of the captured entries logged at level.
func logMessages(entries []logrus.Entry, level logrus.Level) []string {
	var messages []string
	for _, e := range entries {
		if e.Level == level {
			messages = append(messages, e.Message)
		}
	}
	return messages
}

const (
	testEvictionRequested   = "requested"
	testEvictionUnsupported = "unsupported-on-this-platform"
)

func TestEvictionStateRecordsPlatform(t *testing.T) {
	assert.Equal(t, "off", evictionState(false))
	if evictSupported {
		assert.Equal(t, testEvictionRequested, evictionState(true))
		return
	}
	assert.Equal(t, testEvictionUnsupported, evictionState(true))
}

func TestOpenColdFixtureRejectsMissingChunk(t *testing.T) {
	_, _, err := openColdFixture(testLogger(), coldQueryOptions{
		ColdRoot:   t.TempDir(),
		StartChunk: chunk.ID(4),
		NumChunks:  1,
	})
	require.ErrorContains(t, err, "no ledger pack")
}

func TestOpenHotFixtureServesHotChunk(t *testing.T) {
	const ingested = 50
	chunkID := chunk.ID(0)
	packDir, _ := writeSourcePack(t, t.TempDir(), chunkID, ingested)
	hotRoot := ingestHotChunk(t, packDir, ingested)
	first := chunkID.FirstLedger()

	t.Run("uncapped sample", func(t *testing.T) {
		f, release, err := openHotFixture(testLogger(), hotQueryOptions{HotRoot: hotRoot, Chunk: chunkID})
		require.NoError(t, err)
		defer release()

		require.Equal(t, []chunk.ID{chunkID}, f.Chunks)
		assert.Equal(t, first, f.FirstLedger)
		assert.Equal(t, first+ingested-1, f.LastLedger, "the sampled range is what the database holds")

		view, err := f.view()
		require.NoError(t, err)
		defer view.Release()

		scan, err := view.ScanLedgers(first, f.LastLedger)
		require.NoError(t, err)
		seqs := 0
		for _, serr := range scan {
			require.NoError(t, serr)
			seqs++
		}
		assert.Equal(t, ingested, seqs)

		assert.Len(t, view.HotTxHashIndexes(), 1)
	})

	for _, tc := range []struct {
		name          string
		sampleLedgers uint32
		wantLast      uint32
	}{
		{name: "capped sample", sampleLedgers: 10, wantLast: first + 9},
		{name: "cap past what was ingested", sampleLedgers: chunk.LedgersPerChunk, wantLast: first + ingested - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, release, err := openHotFixture(testLogger(), hotQueryOptions{
				HotRoot: hotRoot, Chunk: chunkID, SampleLedgers: tc.sampleLedgers,
			})
			require.NoError(t, err)
			defer release()
			assert.Equal(t, tc.wantLast, f.LastLedger)
		})
	}
}

func TestOpenHotFixtureRejectsMissingDatabase(t *testing.T) {
	_, _, err := openHotFixture(testLogger(), hotQueryOptions{HotRoot: t.TempDir(), Chunk: chunk.ID(3)})
	require.Error(t, err)
}
