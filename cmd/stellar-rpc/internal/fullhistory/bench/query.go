package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/ingest"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/eventstore"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/txhash"
)

// typeTxPage is the transactions-page benchmark's data-type label (and CSV
// basename). Unlike the other three it is not an ingest data type, so it has
// no ingest.DataType* constant.
const typeTxPage = "txpage"

// Driver-report row labels for the query benches' untimed setup work, so a
// report says how much one-off preparation a run paid.
const (
	driverPagePreflight = "txpage_preflight"     // per-ledger tx-count scan; items = txs found
	driverHashSample    = "txhash_corpus_sample" // hash sampling; items = hashes pooled
	driverIndexBuild    = "txhash_index_build"   // cold MPHF build; items = chunks covered
	driverCorpusScan    = "events_corpus_scan"   // events term scan; items = terms kept
)

// queryTypes selects which read benchmarks a run executes.
type queryTypes struct {
	Ledgers, TxPage, Txhash, Events bool
}

func (t queryTypes) any() bool { return t.Ledgers || t.TxPage || t.Txhash || t.Events }

// queryKnobs are the workload knobs shared by both query subcommands.
type queryKnobs struct {
	Types queryTypes
	// Workers are the swept concurrency levels; Iters measured iterations run
	// per worker per sweep cell, after Warmup untimed ones.
	Workers []int
	Iters   int
	Warmup  int
	Seed    uint64
	// LedgersPerRead is the ledgers benchmark's consecutive-read span;
	// PageSize the txpage benchmark's transactions per page; MaxEvents the
	// events benchmark's per-query result cap (0 = uncapped).
	LedgersPerRead int
	PageSize       int
	MaxEvents      int
	// SampleLedgers bounds the per-chunk ledger sample the txhash corpus is
	// drawn from.
	SampleLedgers int
	// Passphrase is the network passphrase transaction-hash verification
	// (txhash.TxReader) runs under; it must match the benchmarked data's
	// network.
	Passphrase string
	// OutDir receives the CSV report.
	OutDir string
}

func (k queryKnobs) validate() error {
	if !k.Types.any() {
		return errors.New("--types must enable at least one of ledgers,txpage,txhash,events")
	}
	if len(k.Workers) == 0 {
		return errors.New("--query-concurrency must list at least one worker count")
	}
	if k.Iters < 1 {
		return fmt.Errorf("--iters must be >= 1, got %d", k.Iters)
	}
	if k.Warmup < 0 {
		return fmt.Errorf("--warmup must be >= 0, got %d", k.Warmup)
	}
	if k.LedgersPerRead < 1 || k.LedgersPerRead > int(chunk.LedgersPerChunk) {
		return fmt.Errorf("--ledgers-per-read must be in [1, %d], got %d", chunk.LedgersPerChunk, k.LedgersPerRead)
	}
	if k.PageSize < 1 {
		return fmt.Errorf("--page-size must be >= 1, got %d", k.PageSize)
	}
	if k.MaxEvents < 0 {
		return fmt.Errorf("--max-events must be >= 0, got %d", k.MaxEvents)
	}
	if k.SampleLedgers < 1 {
		return fmt.Errorf("--sample-ledgers must be >= 1, got %d", k.SampleLedgers)
	}
	if k.Passphrase == "" {
		return errors.New("--network-passphrase must not be empty")
	}
	return nil
}

// sweepSpecFor returns the sweep configuration for one data type's CSV file.
func (k queryKnobs) sweepSpecFor(file string) sweepSpec {
	return sweepSpec{file: file, workers: k.Workers, iters: k.Iters, warmup: k.Warmup, seed: k.Seed}
}

// corpusRNG returns the deterministic RNG the untimed corpus/preflight
// preparation draws from — separate from the sweep workers' RNGs so corpus
// sampling does not shift with the sweep configuration.
func (k queryKnobs) corpusRNG() *rand.Rand {
	return rand.New(rand.NewPCG(k.Seed, k.Seed*seedSalt2)) //nolint:gosec // deterministic input selection, not crypto
}

// registerQueryReport fixes the run's CSV file and row orders on the sink.
// Query row labels depend on the swept concurrency list, so they cannot be
// package-level vocabulary the way the ingest orders are.
func registerQueryReport(sink *csvSink, k queryKnobs) {
	var specs []fileSpec
	var driverRows []string
	for _, t := range []struct {
		name  string
		on    bool
		setup []string
	}{
		{name: ingest.DataTypeLedgers, on: k.Types.Ledgers},
		{name: typeTxPage, on: k.Types.TxPage, setup: []string{driverPagePreflight}},
		{name: ingest.DataTypeTxhash, on: k.Types.Txhash, setup: []string{driverHashSample, driverIndexBuild}},
		{name: ingest.DataTypeEvents, on: k.Types.Events, setup: []string{driverCorpusScan}},
	} {
		if !t.on {
			continue
		}
		driverRows = append(driverRows, t.setup...)
		rows := make([]string, 0, len(k.Workers))
		for _, w := range k.Workers {
			rows = append(rows, fmt.Sprintf("%s_c%d", rowTotal, w))
			driverRows = append(driverRows, fmt.Sprintf("%s_c%d", t.name, w))
		}
		specs = append(specs, fileSpec{name: t.name, rowOrder: rows})
	}
	specs = append(specs, fileSpec{name: fileDriver, rowOrder: driverRows})
	sink.setFileSpecs(specs)
}

// chunkRange lists the n consecutive chunk IDs starting at start.
func chunkRange(start chunk.ID, n int) []chunk.ID {
	out := make([]chunk.ID, n)
	for i := range n {
		out[i] = start + chunk.ID(uint32(i))
	}
	return out
}

// runEventsQuery executes one production events query against r — the
// offsets/EventCount load is part of serving, so callers time around this
// call — and returns the post-filtered result count.
func runEventsQuery(ctx context.Context, r eventstore.Reader, filters []eventstore.Filter, maxEvents int) (int, error) {
	count, err := r.EventCount()
	if err != nil {
		return 0, fmt.Errorf("events count: %w", err)
	}
	payloads, err := eventstore.Query(ctx, r, filters, eventstore.QueryOptions{
		MaxEvents: maxEvents,
		Range:     eventstore.EventIDRange{Start: 0, End: count},
	})
	if err != nil {
		return 0, fmt.Errorf("events query: %w", err)
	}
	return len(payloads), nil
}

// txhashOp returns the op timing one production GetTransaction end-to-end:
// index lookup, ledger fetch, and in-ledger verification (the tx-hash
// analog of the events post-filter). evictPack, when non-nil, drops the
// picked hash's source pack from the page cache before the timed region
// (cold tier only).
func txhashOp(txr *txhash.TxReader, pool *hashPool, evictPack func(chunk.ID) error) queryOp {
	return func(_ context.Context, rng *rand.Rand, record recordFn) error {
		ph := pool.hashes[rng.IntN(len(pool.hashes))]
		if evictPack != nil {
			if err := evictPack(ph.chunk); err != nil {
				return err
			}
		}
		t := time.Now()
		_, found, err := txr.GetTransaction(ph.hash)
		d := time.Since(t)
		if err != nil {
			return fmt.Errorf("txhash lookup: %w", err)
		}
		if !found {
			// Pool hashes were sampled from the benchmarked data, so the
			// production path must find every one of them.
			return fmt.Errorf("txhash lookup: sampled hash %x not found", ph.hash)
		}
		record(rowTotal, d, 1)
		return nil
	}
}
