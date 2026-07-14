package bench

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/config"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
)

// hotOptions configures one hot ingest benchmark run.
type hotOptions struct {
	Source sourceConfig
	// StartChunk..StartChunk+NumChunks-1 are the chunks whose ledgers are
	// driven through the production ingestion loop. A range spanning more than
	// one chunk exercises the loop's boundary handoff (hot DB rotation). Hot
	// ingestion always writes all three data types — one atomic WriteBatch
	// across every hot column family — so there is no Types knob.
	StartChunk chunk.ID
	NumChunks  int
	// NumLedgers caps how many ledgers are ingested from the range's start
	// (0 = the whole range). fsync-per-ledger makes full-chunk runs slow; a
	// cap gives cheap smoke runs without changing what is measured per ledger.
	// Reaching a chunk boundary still requires ingesting the whole first
	// chunk.
	NumLedgers uint32
	// HotRoot is the scratch root the hot RocksDBs are created under (at
	// geometry.NewLayout(HotRoot).HotChunkPath(chunk)). Every chunk DB is
	// opened through the production create bracket, which WIPES any leftover
	// dir first — hot timings are only comparable from a fixed (empty)
	// starting state, so re-runs start clean automatically.
	HotRoot string
	// OutDir receives the CSV report.
	OutDir string
}

func (o hotOptions) validate() error {
	if o.HotRoot == "" {
		return errors.New("--hot-dir is required")
	}
	if o.NumChunks < 1 {
		return fmt.Errorf("--num-chunks must be >= 1, got %d", o.NumChunks)
	}
	if end := uint64(o.StartChunk) + uint64(o.NumChunks) - 1; end > uint64(maxChunkID) {
		return fmt.Errorf("--chunk=%d with --num-chunks=%d ends at chunk %d, past the last valid chunk ID %d",
			uint32(o.StartChunk), o.NumChunks, end, uint32(maxChunkID))
	}
	return nil
}

// runHot benchmarks the production hot ingestion path: the daemon's own
// ingestion loop (via fullhistory.RunBoundedIngestionLoop) over the range's
// ledgers — same per-ledger synced WriteBatch, same chunk-boundary DB rotation
// — into fresh hot DBs opened through a scratch catalog, with a no-op boundary
// so completed chunks are never handed to the cold path. Per-phase percentiles
// come from the loop's HotPhase signals; the run_wall driver row is the whole
// run's wall-clock.
func runHot(ctx context.Context, logger *supportlog.Entry, opts hotOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	// Surface an unwritable --out before the expensive run, not after it.
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fmt.Errorf("create --out dir %s: %w", opts.OutDir, err)
	}
	layout := geometry.NewLayout(opts.HotRoot)
	// Create + fsync the hot root up front — the daemon's own root prep.
	if err := config.PrepareRoots(layout.HotRoot()); err != nil {
		return fmt.Errorf("prepare --hot-dir hot root: %w", err)
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

	first := opts.StartChunk.FirstLedger()
	//nolint:gosec // validate() proved StartChunk+NumChunks-1 <= maxChunkID
	last := (opts.StartChunk + chunk.ID(uint32(opts.NumChunks-1))).LastLedger()
	// Overflow-safe cap: compare against the range's span rather than adding
	// a flag-supplied count to a ledger sequence.
	if span := last - first + 1; opts.NumLedgers > 0 && opts.NumLedgers < span {
		last = first + opts.NumLedgers - 1
	}

	sink := newCSVSink()
	start := time.Now()
	err = fullhistory.RunBoundedIngestionLoop(ctx, fullhistory.BoundedIngestConfig{
		Stream:  boundedStream{inner: backend, first: first, last: last},
		Resume:  first,
		Catalog: cat,
		Logger:  logger,
		Metrics: sink,
		Sink:    sink,
	})
	// The loop cannot tell a complete bounded stream from one that ran dry;
	// the sink's last-committed gauge (set once per ingested ledger) can.
	if err == nil && sink.lastCommittedSeq() != last {
		err = fmt.Errorf("stream ended at seq %d, expected through %d", sink.lastCommittedSeq(), last)
	}
	if err != nil {
		writePartialCSVs(logger, sink, opts.OutDir)
		return err
	}
	sink.observeDriver(driverRunWall, time.Since(start), int(last-first+1))

	sink.logSummary(logger)
	written, err := sink.writeCSVs(opts.OutDir)
	if err != nil {
		return err
	}
	logger.Infof("wrote %d CSVs to %s", len(written), opts.OutDir)
	return nil
}

// boundedStream pins the range a LedgerStream serves: the production ingestion
// loop always requests an unbounded range (it ingests forever), so the bench
// wraps its source with the benchmarked range — the stream ends after last,
// which is what terminates the loop.
type boundedStream struct {
	inner       ledgerbackend.LedgerStream
	first, last uint32
}

func (b boundedStream) RawLedgers(
	ctx context.Context, _ ledgerbackend.Range, opts ...ledgerbackend.StreamOption,
) iter.Seq2[[]byte, error] {
	return b.inner.RawLedgers(ctx, ledgerbackend.BoundedRange(b.first, b.last), opts...)
}
