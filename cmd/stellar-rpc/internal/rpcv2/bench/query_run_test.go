package bench

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"
)

func TestLegSeedIsDistinctPerType(t *testing.T) {
	seen := map[int64]string{}
	for _, qtype := range allQueryTypes {
		seed := legSeed(defaultSeed, qtype)
		if prev, dup := seen[seed]; dup {
			t.Fatalf("%s and %s share leg seed %d", prev, qtype, seed)
		}
		seen[seed] = qtype
	}
	assert.Len(t, seen, len(allQueryTypes))
}

func TestRecordQueryLegAccounting(t *testing.T) {
	res := legResult{
		samples: []cellSample{
			{service: time.Millisecond, scheduled: 2 * time.Millisecond, items: 1, stage: txHashStageFound},
			{service: 2 * time.Millisecond, scheduled: 3 * time.Millisecond, stage: txHashStageMiss},
		},
		lags:      []time.Duration{0, time.Millisecond, 0, time.Millisecond},
		scheduled: 4, dispatched: 3, errs: 1, shed: 1,
		offered: 2 * time.Second, wall: 4 * time.Second, elapsed: 4 * time.Second, drain: 2 * time.Second,
	}
	sink := newSchemaCSVSink(querySpecs([]string{queryTypeTxHash}, []float64{2}))
	recordLeg(sink, queryTypeTxHash, 2, res)
	out := mustWriteCSVs(t, sink)
	driver := readCSV(t, filepath.Join(out, "driver.csv"))
	accounting := readCSV(t, filepath.Join(out, "query-accounting.csv"))
	require.Len(t, driver, 4)
	require.Len(t, accounting, 10)
	for _, tc := range []struct {
		file     string
		suffix   string
		n, items int
		total    int64
	}{
		{fileDriver, "", 1, 2, int64(4 * time.Second)},
		{fileDriver, "_millirps", 1, 2, 1000},
		{fileDriver, "_shed", 1, 1, 0},
		{fileDriver, "_lag", 4, 4, int64(2 * time.Millisecond)},
		{fileQueryAccounting, "_target_millirps", 1, 0, 2000},
		{fileQueryAccounting, "_completion_millirps", 1, 0, 500},
		{fileQueryAccounting, "_scheduled", 1, 4, 0},
		{fileQueryAccounting, "_dispatched", 1, 3, 0},
		{fileQueryAccounting, "_successful", 1, 2, 0},
		{fileQueryAccounting, "_failed", 1, 1, 0},
		{fileQueryAccounting, "_shed", 1, 1, 0},
		{fileQueryAccounting, "_arrival", 1, 0, int64(2 * time.Second)},
		{fileQueryAccounting, "_elapsed", 1, 0, int64(4 * time.Second)},
		{fileQueryAccounting, "_drain", 1, 0, int64(2 * time.Second)},
	} {
		name := "txhash_r2" + tc.suffix
		rows := accounting
		if tc.file == fileDriver {
			rows = driver
		}
		require.Contains(t, rows, name)
		assert.EqualValues(t, tc.n, rows[name]["n"], name)
		assert.EqualValues(t, tc.items, rows[name]["n_items"], name)
		assert.Equal(t, tc.total, rows[name]["total_ns"], name)
	}
	assert.Equal(t, driver["txhash_r2_shed"], accounting["txhash_r2_shed"])
	latency := readCSV(t, filepath.Join(out, "txhash.csv"))
	assert.EqualValues(t, 2, latency["total_r2"]["n"])
	assert.EqualValues(t, 2, latency["service_r2"]["n"])
	assert.EqualValues(t, 1, latency["found_r2"]["n"])
	assert.EqualValues(t, 1, latency["miss_r2"]["n"])
}

func TestRunQueryLegPreservesRequestFailures(t *testing.T) {
	for _, allFailed := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial-success", true: "all-failed"}[allFailed], func(t *testing.T) {
			var calls atomic.Int32
			req := func(*rand.Rand) (cellSample, error) {
				if calls.Add(1)%2 == 0 || allFailed {
					return cellSample{}, errors.New("read failed")
				}
				return cellSample{service: time.Microsecond, items: 1}, nil
			}
			var output bytes.Buffer
			logger := supportlog.New()
			logger.SetLevel(supportlog.InfoLevel)
			logger.SetOutput(&output)
			sink := newSchemaCSVSink(querySpecs([]string{queryTypeLedgers}, []float64{1000}))
			p := queryPlan{Duration: 4 * time.Millisecond}
			err := runQueryLeg(context.Background(), logger, nil, p, sink, queryTypeLedgers, 1000, req)
			require.ErrorContains(t, err, "requests failed")
			out := t.TempDir()
			writePartialCSVs(logger, sink, out)
			driver := readCSV(t, filepath.Join(out, "driver.csv"))
			accounting := readCSV(t, filepath.Join(out, "query-accounting.csv"))
			failed, successful := 2, 2
			if allFailed {
				failed, successful = 4, 0
				assert.NoFileExists(t, filepath.Join(out, "ledgers.csv"))
			} else {
				latency := readCSV(t, filepath.Join(out, "ledgers.csv"))
				assert.EqualValues(t, successful, latency["total_r1000"]["n"])
			}
			require.Len(t, accounting, 10)
			assert.EqualValues(t, 4, accounting["ledgers_r1000_scheduled"]["n_items"])
			assert.EqualValues(t, 4, accounting["ledgers_r1000_dispatched"]["n_items"])
			assert.EqualValues(t, successful, accounting["ledgers_r1000_successful"]["n_items"])
			assert.EqualValues(t, failed, accounting["ledgers_r1000_failed"]["n_items"])
			assert.EqualValues(t, 1, driver["ledgers_r1000_shed"]["n"])
			assert.EqualValues(t, 0, driver["ledgers_r1000_shed"]["n_items"])
			assert.Equal(t, driver["ledgers_r1000_shed"], accounting["ledgers_r1000_shed"])
			assert.EqualValues(t, successful, driver["ledgers_r1000_millirps"]["n_items"])
			assert.EqualValues(t, successful, driver["ledgers_r1000"]["n_items"])
			assert.Contains(t, output.String(), "outcome: scheduled=4 dispatched=4")
			assert.Contains(t, output.String(), "PARTIAL CSVs")
		})
	}
}

func TestQueryLegWarningsUseSuccessfulSubgroups(t *testing.T) {
	var output bytes.Buffer
	logger := supportlog.New()
	logger.SetLevel(supportlog.InfoLevel)
	logger.SetOutput(&output)
	res := legResult{scheduled: 200, dispatched: 150, errs: 40, shed: 50, offered: time.Second, elapsed: time.Second}
	for i := range 110 {
		stage := txHashStageFound
		if i < 10 {
			stage = txHashStageMiss
		}
		res.samples = append(res.samples, cellSample{scheduled: time.Millisecond, stage: stage})
	}
	logQueryLeg(logger, queryTypeTxHash, 200, res)
	assert.Contains(t, output.String(), "shed 50 requests")
	assert.Contains(t, output.String(), "miss_r200 has 10 successful samples")
	assert.NotContains(t, output.String(), "found_r200 has")
	assert.NotContains(t, output.String(), "total_r200 has")
	assert.Less(t, strings.Index(output.String(), "outcome:"), strings.Index(output.String(), "p50="))
	output.Reset()
	res.samples = nil
	logQueryLeg(logger, queryTypeTxHash, 200, res)
	for _, label := range []string{"total_r200", "found_r200", "miss_r200"} {
		assert.Contains(t, output.String(), label+" has 0 successful samples")
	}
}

func TestAchievedMilliRPS(t *testing.T) {
	assert.Equal(t, time.Duration(1250), achievedMilliRPS(5, 4*time.Second))
	assert.Zero(t, achievedMilliRPS(0, time.Second))
	assert.Zero(t, achievedMilliRPS(1, 0))
}
