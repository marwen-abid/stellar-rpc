package bench

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// recordFn records one row sample for the current sweep cell. The sweep
// runner binds it to the data type's CSV file, appends the cell's
// concurrency suffix to the row label, and drops samples recorded during
// warmup iterations.
type recordFn func(row string, d time.Duration, items int)

// queryOp runs one query iteration end-to-end against a production read
// path, timing its measured region itself and reporting it via record —
// untimed setup such as page-cache eviction is simply never recorded. rng is
// worker-private; anything else an op closure touches must be safe for
// concurrent use across workers.
type queryOp func(ctx context.Context, rng *rand.Rand, record recordFn) error

// rowTotal is the row label ops record their end-to-end duration under; the
// sweep runner suffixes it into "total_c<workers>".
const rowTotal = "total"

// Multipliers folding the worker count into each worker's PCG seed, so
// different sweep cells draw independent input sequences even though they
// share --seed.
const (
	seedCellSalt1 = 1000003
	seedCellSalt2 = 73
	seedSalt2     = 7919
)

// sweepSpec is one data type's concurrency sweep: the CSV file its rows land
// in, the worker counts to sweep, and the per-worker iteration budget.
type sweepSpec struct {
	file    string
	workers []int
	iters   int // measured iterations per worker per cell
	warmup  int // untimed iterations per worker per cell (hot tier only)
	seed    uint64
}

// runSweep drives op through spec: for each swept worker count W it runs W
// goroutines × spec.iters measured iterations (after spec.warmup untimed
// ones per worker), closed-loop with no think time. Each cell's wall-clock
// and completed-iteration count land in driver.csv under "<file>_c<W>" — the
// run's throughput numerator/denominator. Warmup shares the measured phase's
// RNGs on purpose: reseeding would replay the warmup's draws against a
// freshly warmed cache.
func runSweep(ctx context.Context, sink *csvSink, spec sweepSpec, op queryOp) error {
	for _, w := range spec.workers {
		if err := runSweepCell(ctx, sink, spec, w, op); err != nil {
			return fmt.Errorf("%s at concurrency %d: %w", spec.file, w, err)
		}
	}
	return nil
}

func runSweepCell(ctx context.Context, sink *csvSink, spec sweepSpec, workers int, op queryOp) error {
	rngs := make([]*rand.Rand, workers)
	for i := range rngs {
		//nolint:gosec // deterministic input selection, not crypto; i and workers are small positives
		rngs[i] = rand.New(rand.NewPCG(
			spec.seed+uint64(i)+uint64(workers)*seedCellSalt1,
			spec.seed*seedSalt2+uint64(i)+uint64(workers)*seedCellSalt2,
		))
	}
	suffix := fmt.Sprintf("_c%d", workers)
	var completed atomic.Int64
	run := func(measured bool, iters int) error {
		g, gctx := errgroup.WithContext(ctx)
		for i := range workers {
			g.Go(func() error {
				record := func(string, time.Duration, int) {}
				if measured {
					record = func(row string, d time.Duration, items int) {
						sink.observe(spec.file, row+suffix, d, items)
					}
				}
				for range iters {
					if err := gctx.Err(); err != nil {
						return err
					}
					if err := op(gctx, rngs[i], record); err != nil {
						return err
					}
					if measured {
						completed.Add(1)
					}
				}
				return nil
			})
		}
		return g.Wait()
	}
	if spec.warmup > 0 {
		if err := run(false, spec.warmup); err != nil {
			return fmt.Errorf("warmup: %w", err)
		}
	}
	start := time.Now()
	if err := run(true, spec.iters); err != nil {
		return err
	}
	sink.observeDriver(spec.file+suffix, time.Since(start), int(completed.Load()))
	return nil
}
