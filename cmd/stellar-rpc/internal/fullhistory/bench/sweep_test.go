package bench

import (
	"context"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunSweepRecordsCellsAndDriver drives a fake op through a two-cell
// sweep and checks the report shape: per-cell rows carry the _c<W> suffix
// with W×iters samples each, warmup iterations run the op but record
// nothing, and driver.csv gets one wall row per cell whose items count the
// completed measured iterations.
func TestRunSweepRecordsCellsAndDriver(t *testing.T) {
	sink := newCSVSink()
	var calls atomic.Int32
	op := func(_ context.Context, _ *rand.Rand, record recordFn) error {
		calls.Add(1)
		record(rowTotal, time.Millisecond, 2)
		return nil
	}
	spec := sweepSpec{file: "demo", workers: []int{1, 3}, iters: 4, warmup: 2, seed: 7}
	require.NoError(t, runSweep(context.Background(), sink, spec, op))

	// cell c1: 1×(2 warmup + 4 measured); cell c3: 3×(2 + 4).
	assert.EqualValues(t, 6+18, calls.Load())

	outDir := t.TempDir()
	_, err := sink.writeCSVs(outDir)
	require.NoError(t, err)

	demo := readCSV(t, filepath.Join(outDir, "demo.csv"))
	require.Contains(t, demo, "total_c1")
	assert.EqualValues(t, 4, demo["total_c1"]["n"], "warmup samples must not be recorded")
	assert.EqualValues(t, 8, demo["total_c1"]["n_items"])
	require.Contains(t, demo, "total_c3")
	assert.EqualValues(t, 12, demo["total_c3"]["n"])
	assert.EqualValues(t, 24, demo["total_c3"]["n_items"])

	driver := readCSV(t, filepath.Join(outDir, "driver.csv"))
	require.Contains(t, driver, "demo_c1")
	assert.EqualValues(t, 1, driver["demo_c1"]["n"])
	assert.EqualValues(t, 4, driver["demo_c1"]["n_items"])
	require.Contains(t, driver, "demo_c3")
	assert.EqualValues(t, 12, driver["demo_c3"]["n_items"])
}

// TestRunSweepPropagatesOpErrors: a failing op aborts the sweep with the
// cell's file and concurrency in the error.
func TestRunSweepPropagatesOpErrors(t *testing.T) {
	sink := newCSVSink()
	boom := errors.New("boom")
	op := func(context.Context, *rand.Rand, recordFn) error { return boom }
	err := runSweep(context.Background(), sink, sweepSpec{file: "demo", workers: []int{2}, iters: 1, seed: 1}, op)
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "demo at concurrency 2")
}
