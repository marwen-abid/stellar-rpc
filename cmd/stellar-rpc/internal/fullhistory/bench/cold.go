// Package bench implements the full-history ingestion benchmarks behind the
// stellar-rpc bench-ingest subcommand: drivers that run the PRODUCTION
// ingestion paths — backfill.RunBackfill (cold: chunk freezes plus the
// cross-chunk txhash index builds, on the daemon's own scheduler) and the
// daemon's hot ingestion loop (via fullhistory.RunBoundedIngestionLoop) — and a
// csvSink that aggregates the MetricSink and observability.Metrics signals
// those paths already emit into per-stage percentile CSV reports.
package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/backfill"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/catalog"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/config"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
)

// maxChunkID is the last chunk ID whose LastLedger fits in a uint32 ledger
// sequence (see chunk.ID.LastLedger).
const maxChunkID = chunk.ID(math.MaxUint32/chunk.LedgersPerChunk - 1)

// coldOptions configures one cold ingest benchmark run.
type coldOptions struct {
	Source sourceConfig
	// StartChunk..StartChunk+NumChunks-1 is the backfilled range. RunBackfill
	// materializes the full artifact set for it — ledger packs, txhash .bins,
	// events segments, plus the cross-chunk txhash index builds — exactly what
	// the daemon's startup backfill produces.
	StartChunk chunk.ID
	NumChunks  int
	// Workers sizes RunBackfill's single bounded pool, shared by chunk freezes
	// and index builds.
	Workers int
	// ColdRoot is the output root the cold artifacts land under, laid out by
	// geometry.NewLayout. It is scratch: the run's catalog is a fresh temp-dir
	// scratch (deleted afterwards), so re-runs re-plan from empty and
	// overwrite freely.
	ColdRoot string
	// OutDir receives the CSV report.
	OutDir string
}

func (o coldOptions) validate() error {
	if o.NumChunks < 1 {
		return fmt.Errorf("--num-chunks must be >= 1, got %d", o.NumChunks)
	}
	if o.Workers < 1 {
		return fmt.Errorf("--workers must be >= 1, got %d", o.Workers)
	}
	// uint64 so StartChunk+NumChunks-1 cannot itself wrap before the compare.
	if end := uint64(o.StartChunk) + uint64(o.NumChunks) - 1; end > uint64(maxChunkID) {
		return fmt.Errorf("--chunk=%d with --num-chunks=%d ends at chunk %d, past the last valid chunk ID %d",
			uint32(o.StartChunk), o.NumChunks, end, uint32(maxChunkID))
	}
	if o.ColdRoot == "" {
		return errors.New("--cold-out-dir is required")
	}
	// Refuse re-packing a source pack tree in place: the backfill always
	// materializes ledger packs, the cold ledger writer overwrites its
	// destination, and destination == source would corrupt the pack mid-read.
	if o.Source.Kind == sourcePack {
		outLedgers := geometry.NewLayout(o.ColdRoot).LedgersRoot()
		if samePath(o.Source.PackDir, outLedgers) {
			return fmt.Errorf("--cold-out-dir's ledgers tree (%s) must differ from --pack-dir", outLedgers)
		}
	}
	return nil
}

// runCold benchmarks the production cold ingestion path: one
// backfill.RunBackfill call over [StartChunk, StartChunk+NumChunks) — the same
// plan-and-execute the daemon runs at startup, against a fresh scratch catalog
// so the whole range is a clean backfill from empty — with the CSV sink
// receiving both the per-stage MetricSink signals (via WriteColdChunk,
// unchanged) and the scheduler's observability signals (backfill wall, index
// rebuilds). On success it writes the CSV report and logs a per-row summary
// plus the run's effective chunk concurrency (sum(chunk_total)/total_wall).
func runCold(ctx context.Context, logger *supportlog.Entry, opts coldOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	// Surface an unwritable --out before the run.
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fmt.Errorf("create --out dir %s: %w", opts.OutDir, err)
	}
	// Create and fsync the write roots up front — the daemon's own root prep.
	layout := geometry.NewLayout(opts.ColdRoot)
	if err := config.PrepareRoots(
		layout.LedgersRoot(), layout.EventsRoot(), layout.TxHashRawRoot(), layout.TxHashIndexRoot(),
	); err != nil {
		return fmt.Errorf("prepare --cold-out-dir write roots: %w", err)
	}

	cat, releaseCat, err := openScratchCatalog(layout, logger)
	if err != nil {
		return err
	}
	defer releaseCat()

	backend, release, err := openSource(ctx, opts.Source)
	if err != nil {
		return err
	}
	defer release()

	sink := newCSVSink()
	//nolint:gosec // validate() proved StartChunk+NumChunks-1 <= maxChunkID
	end := opts.StartChunk + chunk.ID(uint32(opts.NumChunks-1))

	start := time.Now()
	err = backfill.RunBackfill(ctx, backfill.ExecConfig{
		Catalog: cat,
		Logger:  logger,
		Metrics: sink,
		Process: backfill.ProcessConfig{Sink: sink, Backend: backend},
		Workers: opts.Workers,
		// Benchmarks measure one clean attempt; retries would fold failure +
		// backoff time into the samples.
		MaxRetries: 0,
	}, opts.StartChunk, end)
	if err != nil {
		writePartialCSVs(logger, sink, opts.OutDir)
		return fmt.Errorf("backfill [%s,%s]: %w", opts.StartChunk, end, err)
	}
	totalWall := time.Since(start)

	sink.logSummary(logger)
	logColdWall(logger, sink, opts.NumChunks, totalWall)
	written, err := sink.writeCSVs(opts.OutDir)
	if err != nil {
		return err
	}
	logger.Infof("wrote %d CSVs to %s", len(written), opts.OutDir)
	return nil
}

// openScratchCatalog opens a fresh, run-scoped catalog in a temp dir, bound to
// layout. Fresh-per-run is what makes every run a clean backfill from empty
// (durable states would otherwise self-skip finished work on a re-run); the
// release func closes the catalog and deletes the temp dir.
func openScratchCatalog(layout geometry.Layout, logger *supportlog.Entry) (*catalog.Catalog, func(), error) {
	dir, err := os.MkdirTemp("", "bench-ingest-catalog-")
	if err != nil {
		return nil, nil, fmt.Errorf("create scratch catalog dir: %w", err)
	}
	txLayout, err := geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	cat, err := catalog.Open(filepath.Join(dir, "catalog"), layout, txLayout, logger)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("open scratch catalog: %w", err)
	}
	return cat, func() {
		_ = cat.Close()
		_ = os.RemoveAll(dir)
	}, nil
}

// logColdWall logs the run's total wall-clock and, for multi-chunk runs, the
// effective chunk concurrency (sum of the engine's per-chunk totals over the
// total wall).
func logColdWall(logger *supportlog.Entry, sink *csvSink, numChunks int, totalWall time.Duration) {
	if numChunks > 1 && totalWall > 0 {
		sumChunkTotal := sink.sumDriver(driverChunkTotal)
		logger.Infof("total wall = %s (sum(chunk_total)/total = %.2fx effective concurrency)",
			totalWall.Round(time.Millisecond), float64(sumChunkTotal)/float64(totalWall))
		return
	}
	logger.Infof("total wall = %s", totalWall.Round(time.Millisecond))
}

// samePath reports whether a and b resolve to the same directory. Falls back
// to an abs-path compare when either does not exist yet.
func samePath(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	absA, _ := filepath.Abs(a)
	absB, _ := filepath.Abs(b)
	return absA == absB
}
