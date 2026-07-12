package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/ingest"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/hotchunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/txhash"
)

// hotQueryOptions configures one hot query benchmark run.
type hotQueryOptions struct {
	queryKnobs

	// HotRoot is the root the benchmarked hot RocksDB lives under (at
	// geometry.NewLayout(HotRoot).HotChunkPath(Chunk)) — the tree a
	// bench-ingest hot run's --hot-dir populated, or a deployment's data
	// dir. The DB is opened read-write (hotchunk.OpenExisting): a read-only
	// open is structurally ledgers-only — no events facade (#834) — and the
	// handle that serves hot queries in production is the live read-write
	// one. Nothing is written; create-if-missing stays off.
	HotRoot string
	// Chunk is the single chunk whose hot DB is benchmarked.
	Chunk chunk.ID
}

func (o hotQueryOptions) validate() error {
	if err := o.queryKnobs.validate(); err != nil {
		return err
	}
	if o.HotRoot == "" {
		return errors.New("--hot-dir is required")
	}
	if o.Chunk > maxChunkID {
		return fmt.Errorf("--chunk=%d is past the last valid chunk ID %d", uint32(o.Chunk), uint32(maxChunkID))
	}
	return nil
}

// runQueryHot benchmarks the production hot read paths over an existing
// per-chunk hot RocksDB, shared by all workers — the live serving shape,
// eventstore warmup included at open. Each sweep cell runs --warmup untimed
// iterations per worker first so the block cache reaches steady state.
func runQueryHot(ctx context.Context, logger *supportlog.Entry, opts hotQueryOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	// Surface an unwritable --out before the expensive run, not after it.
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fmt.Errorf("create --out dir %s: %w", opts.OutDir, err)
	}
	dbPath := geometry.NewLayout(opts.HotRoot).HotChunkPath(opts.Chunk)
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("hot DB dir (produce one with bench-ingest hot): %w", err)
	}
	db, err := hotchunk.OpenExisting(dbPath, opts.Chunk, logger)
	if err != nil {
		return fmt.Errorf("open hot DB %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	last, committed, err := db.Ledgers().LastSeq()
	if err != nil {
		return fmt.Errorf("hot DB %s last seq: %w", dbPath, err)
	}
	if !committed {
		return fmt.Errorf("hot DB %s has no committed ledgers", dbPath)
	}

	sink := newCSVSink()
	registerQueryReport(sink, opts.queryKnobs)
	if err := runHotTypes(ctx, db, opts.Chunk.FirstLedger(), last, opts, sink); err != nil {
		writePartialCSVs(logger, sink, opts.OutDir)
		return err
	}
	sink.logSummary(logger)
	written, err := sink.writeCSVs(opts.OutDir)
	if err != nil {
		return err
	}
	logger.Infof("wrote %d CSVs to %s", len(written), opts.OutDir)
	return nil
}

// runHotTypes runs each enabled data type's sweep in the fixed report
// order against the shared DB, preparing untimed corpus/preflight
// inputs first. [first, last] is the DB's committed ledger span.
func runHotTypes(
	ctx context.Context, db *hotchunk.DB, first, last uint32, opts hotQueryOptions, sink *csvSink,
) error {
	if opts.Types.Ledgers {
		op, err := hotLedgersOp(db, first, last, opts.LedgersPerRead)
		if err != nil {
			return err
		}
		if err := runSweep(ctx, sink, opts.sweepSpecFor(ingest.DataTypeLedgers), op); err != nil {
			return err
		}
	}
	if opts.Types.TxPage {
		op, err := hotTxPageOp(db, first, last, opts.PageSize, sink)
		if err != nil {
			return err
		}
		if err := runSweep(ctx, sink, opts.sweepSpecFor(typeTxPage), op); err != nil {
			return err
		}
	}
	if opts.Types.Txhash {
		op, err := hotTxhashOp(db, first, last, opts, sink)
		if err != nil {
			return err
		}
		if err := runSweep(ctx, sink, opts.sweepSpecFor(ingest.DataTypeTxhash), op); err != nil {
			return err
		}
	}
	if opts.Types.Events {
		op, err := hotEventsOp(ctx, db, opts.MaxEvents, sink)
		if err != nil {
			return err
		}
		if err := runSweep(ctx, sink, opts.sweepSpecFor(ingest.DataTypeEvents), op); err != nil {
			return err
		}
	}
	return nil
}

// hotLedgersOp returns the op timing one hot ledger-range read off the
// shared store: n consecutive ledgers from a random committed start.
func hotLedgersOp(db *hotchunk.DB, first, last uint32, n int) (queryOp, error) {
	span := int(last - first + 1)
	if span < n {
		return nil, fmt.Errorf("hot DB holds %d ledgers, fewer than --ledgers-per-read=%d", span, n)
	}
	store := db.Ledgers()
	return func(_ context.Context, rng *rand.Rand, record recordFn) error {
		start := first + uint32(rng.IntN(span-n+1)) //nolint:gosec // in-span offset
		t := time.Now()
		if err := readLedgerSpan(store, start, n); err != nil {
			return err
		}
		record(rowTotal, time.Since(t), n)
		return nil
	}, nil
}

// hotTxPageOp preflights the committed span's per-ledger tx counts
// (untimed) and returns the op timing one hot transactions-page read.
func hotTxPageOp(db *hotchunk.DB, first, last uint32, pageSize int, sink *csvSink) (queryOp, error) {
	store := db.Ledgers()
	t := time.Now()
	cursors, err := buildPageCursors(store, first, last)
	if err != nil {
		return nil, err
	}
	sink.observeDriver(driverPagePreflight, time.Since(t), cursors.total)
	if cursors.total < pageSize {
		return nil, fmt.Errorf("hot DB holds %d transactions, fewer than --page-size=%d", cursors.total, pageSize)
	}
	return func(_ context.Context, rng *rand.Rand, record recordFn) error {
		seq, skip := cursors.draw(rng, pageSize)
		t := time.Now()
		if err := walkPage(store, seq, skip, pageSize); err != nil {
			return err
		}
		record(rowTotal, time.Since(t), pageSize)
		return nil
	}, nil
}

// hotTxhashOp samples the hash corpus from the DB's own ledgers (untimed)
// and returns the op timing one production GetTransaction against the hot
// index + hot ledger store.
func hotTxhashOp(db *hotchunk.DB, first, last uint32, opts hotQueryOptions, sink *csvSink) (queryOp, error) {
	pool := &hashPool{}
	t := time.Now()
	if err := samplePoolHashes(db.Ledgers(), first, last, opts.SampleLedgers, opts.corpusRNG(), pool); err != nil {
		return nil, err
	}
	if len(pool.hashes) == 0 {
		return nil, errors.New("txhash corpus: no transactions in the sampled ledgers; raise --sample-ledgers")
	}
	sink.observeDriver(driverHashSample, time.Since(t), len(pool.hashes))
	txr, err := txhash.NewTxReader([]txhash.HashIndex{db.Txhash()}, nil, db.Ledgers(), opts.Passphrase)
	if err != nil {
		return nil, err
	}
	return txhashOp(txr, pool, nil), nil
}

// hotEventsOp derives the query corpus from the DB's own events (untimed)
// and returns the op timing one production events query — index probe and
// post-filter included — against the shared hot store.
func hotEventsOp(ctx context.Context, db *hotchunk.DB, maxEvents int, sink *csvSink) (queryOp, error) {
	store := db.Events()
	t := time.Now()
	corpus, err := buildEventCorpus(ctx, store)
	if err != nil {
		return nil, err
	}
	sink.observeDriver(driverCorpusScan, time.Since(t), len(corpus.terms))
	return func(ctx context.Context, rng *rand.Rand, record recordFn) error {
		filters := corpus.draw(rng)
		t := time.Now()
		n, err := runEventsQuery(ctx, store, filters, maxEvents)
		if err != nil {
			return err
		}
		record(rowTotal, time.Since(t), n)
		return nil
	}, nil
}
