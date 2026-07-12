package bench

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/eventstore"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/ledger"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/txhash"
)

// coldQueryOptions configures one cold query benchmark run.
type coldQueryOptions struct {
	queryKnobs

	// ColdRoot is the cold artifact tree to read, laid out by
	// geometry.NewLayout — the tree a bench-ingest cold run's --cold-out-dir
	// produced, or a deployment's cold root. It is only ever read.
	ColdRoot string
	// StartChunk..StartChunk+NumChunks-1 are the benchmarked chunks; each
	// iteration draws its inputs from one of them at random.
	StartChunk chunk.ID
	NumChunks  int
	// TxhashIndex optionally points at an existing cold tx-hash MPHF (.idx).
	// When empty, the driver builds one over the benchmarked chunks' .bin
	// files into a temp dir as untimed setup.
	TxhashIndex string
	// ReaderConcurrency is the cold events reader's packfile read fan-out.
	ReaderConcurrency int
}

func (o coldQueryOptions) validate() error {
	if err := o.queryKnobs.validate(); err != nil {
		return err
	}
	if o.ColdRoot == "" {
		return errors.New("--cold-dir is required")
	}
	if err := validateChunkRange(o.StartChunk, o.NumChunks); err != nil {
		return err
	}
	if o.ReaderConcurrency < 0 {
		return fmt.Errorf("--reader-concurrency must be >= 0, got %d", o.ReaderConcurrency)
	}
	return nil
}

// validateChunkRange rejects an empty chunk range and a range that ends past
// the last valid chunk ID.
func validateChunkRange(start chunk.ID, numChunks int) error {
	if numChunks < 1 {
		return fmt.Errorf("--num-chunks must be >= 1, got %d", numChunks)
	}
	// uint64 so start+numChunks-1 cannot itself wrap before the compare.
	if end := uint64(start) + uint64(numChunks) - 1; end > uint64(maxChunkID) {
		return fmt.Errorf("--start-chunk=%d with --num-chunks=%d ends at chunk %d, past the last valid chunk ID %d",
			uint32(start), numChunks, end, uint32(maxChunkID))
	}
	return nil
}

// runQueryCold benchmarks the production cold read paths over frozen chunk
// artifacts: per --types it sweeps the configured concurrency levels,
// evicting the touched files from the OS page cache before every iteration
// so reads pay real device latency, and reports per-cell percentile CSVs.
func runQueryCold(ctx context.Context, logger *supportlog.Entry, opts coldQueryOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	// Surface an unwritable --out before the expensive run, not after it.
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fmt.Errorf("create --out dir %s: %w", opts.OutDir, err)
	}
	if !evictionWorks {
		logger.Warn("page-cache eviction is unsupported on this OS; cold latencies will reflect a warm page cache")
	}
	sink := newCSVSink()
	registerQueryReport(sink, opts.queryKnobs)
	err := runColdTypes(ctx, logger, geometry.NewLayout(opts.ColdRoot), opts, sink)
	recordPeakRSS(logger, sink, readPeakRSS)
	if err != nil {
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

// runColdTypes runs each enabled data type's sweep in the fixed report
// order, preparing its corpus/preflight inputs untimed first.
func runColdTypes(
	ctx context.Context, logger *supportlog.Entry, layout geometry.Layout, opts coldQueryOptions, sink *csvSink,
) error {
	chunks := chunkRange(opts.StartChunk, opts.NumChunks)
	if opts.Types.Ledgers || opts.Types.TxPage || opts.Types.Txhash {
		packs, err := inspectColdPacks(layout, chunks)
		if err != nil {
			return err
		}
		if err := runColdPackTypes(ctx, logger, layout, packs, opts, sink); err != nil {
			return err
		}
	}
	if opts.Types.Events {
		corpora, err := coldEventsSetup(ctx, layout, chunks, opts, sink)
		if err != nil {
			return err
		}
		op := coldEventsOp(corpora, opts.MaxEvents, opts.ReaderConcurrency)
		if err := runSweep(ctx, sink, opts.sweepSpecFor(coldTypeEvents), op); err != nil {
			return err
		}
	}
	return nil
}

// runColdPackTypes runs the three ledger-pack-backed sweeps (ledgers,
// txpage, txhash).
func runColdPackTypes(
	ctx context.Context,
	logger *supportlog.Entry,
	layout geometry.Layout,
	packs []coldPack,
	opts coldQueryOptions,
	sink *csvSink,
) error {
	if opts.Types.Ledgers {
		op, err := coldLedgersOp(packs, opts.LedgersPerRead)
		if err != nil {
			return err
		}
		if err := runSweep(ctx, sink, opts.sweepSpecFor(coldTypeLedgers), op); err != nil {
			return err
		}
	}
	if opts.Types.TxPage {
		cursors, err := coldTxPageSetup(logger, packs, opts.PageSize, sink)
		if err != nil {
			return err
		}
		if err := runSweep(ctx, sink, opts.sweepSpecFor(typeTxPage), coldTxPageOp(cursors, opts.PageSize)); err != nil {
			return err
		}
	}
	if opts.Types.Txhash {
		op, cleanup, err := coldTxhashSetup(ctx, logger, layout, packs, opts, sink)
		if err != nil {
			return err
		}
		err = runSweep(ctx, sink, opts.sweepSpecFor(coldTypeTxhash), op)
		cleanup()
		if err != nil {
			return err
		}
	}
	return nil
}

// coldPack is one benchmarked chunk's ledger pack with its actual ledger
// span read from the pack trailer.
type coldPack struct {
	id    chunk.ID
	path  string
	first uint32
	last  uint32
}

// inspectColdPacks reads each benchmarked chunk's ledger-pack span up front,
// so a missing or corrupt artifact fails before any sweep runs.
func inspectColdPacks(layout geometry.Layout, chunks []chunk.ID) ([]coldPack, error) {
	packs := make([]coldPack, 0, len(chunks))
	for _, c := range chunks {
		path := layout.LedgerPackPath(c)
		cr, err := ledger.OpenColdReader(path)
		if err != nil {
			return nil, err
		}
		last, lerr := cr.LastSeq()
		if err := errors.Join(lerr, cr.Close()); err != nil {
			return nil, fmt.Errorf("ledger pack %s: %w", path, err)
		}
		// A chunk pack's first ledger is fixed by geometry; the reader only
		// exposes its last sequence.
		packs = append(packs, coldPack{id: c, path: path, first: c.FirstLedger(), last: last})
	}
	return packs, nil
}

// ledgerReader is the span-read seam both ledger store tiers expose.
type ledgerReader interface {
	GetLedgerRaw(seq uint32) ([]byte, error)
	IterateLedgers(start, end uint32) iter.Seq2[ledger.Entry, error]
}

// readLedgerSpan reads n consecutive raw ledgers from r starting at start —
// a single point read for n==1, the range iterator otherwise — verifying
// every ledger arrived non-empty.
func readLedgerSpan(r ledgerReader, start uint32, n int) error {
	if n == 1 {
		raw, err := r.GetLedgerRaw(start)
		if err != nil {
			return fmt.Errorf("ledger %d: %w", start, err)
		}
		if len(raw) == 0 {
			return fmt.Errorf("ledger %d: empty", start)
		}
		return nil
	}
	seen := 0
	end := start + uint32(n) - 1 //nolint:gosec // n <= LedgersPerChunk, validated
	for e, err := range r.IterateLedgers(start, end) {
		if err != nil {
			return fmt.Errorf("iterate from %d: %w", start, err)
		}
		if len(e.Bytes) == 0 {
			return fmt.Errorf("ledger %d: empty", e.Seq)
		}
		seen++
	}
	if seen != n {
		return fmt.Errorf("iterate from %d: got %d ledgers, want %d", start, seen, n)
	}
	return nil
}

// coldLedgersOp returns the op timing one cold ledger-range read: open the
// pack's production reader and read n consecutive ledgers from a random
// start. The pack is evicted from the page cache first, untimed.
func coldLedgersOp(packs []coldPack, n int) (queryOp, error) {
	eligible := make([]coldPack, 0, len(packs))
	for _, p := range packs {
		if int(p.last-p.first+1) >= n {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no benchmarked chunk holds %d consecutive ledgers (--ledgers-per-read)", n)
	}
	return func(_ context.Context, rng *rand.Rand, record recordFn) error {
		p := eligible[rng.IntN(len(eligible))]
		if err := evictFromPageCache(p.path); err != nil {
			return err
		}
		start := p.first + uint32(rng.IntN(int(p.last-p.first+1)-n+1)) //nolint:gosec // in-span offset
		t := time.Now()
		cr, err := ledger.OpenColdReader(p.path)
		if err != nil {
			return err
		}
		defer func() { _ = cr.Close() }()
		if err := readLedgerSpan(cr, start, n); err != nil {
			return err
		}
		record(rowTotal, time.Since(t), n)
		return nil
	}, nil
}

// chunkPageCursors pairs one chunk's pack with its preflighted page cursors.
type chunkPageCursors struct {
	pack    coldPack
	cursors *pageCursors
}

// coldTxPageSetup preflights every benchmarked chunk's per-ledger tx counts
// (untimed) and drops chunks too sparse to hold one page.
func coldTxPageSetup(
	logger *supportlog.Entry, packs []coldPack, pageSize int, sink *csvSink,
) ([]chunkPageCursors, error) {
	out := make([]chunkPageCursors, 0, len(packs))
	for _, p := range packs {
		cr, err := ledger.OpenColdReader(p.path)
		if err != nil {
			return nil, err
		}
		t := time.Now()
		cursors, cerr := buildPageCursors(cr, p.first, p.last)
		if err := errors.Join(cerr, cr.Close()); err != nil {
			return nil, fmt.Errorf("chunk %d: %w", uint32(p.id), err)
		}
		sink.observeDriver(driverPagePreflight, time.Since(t), cursors.total)
		if cursors.total < pageSize {
			logger.Infof("txpage: skipping chunk %d (%d transactions < --page-size=%d)",
				uint32(p.id), cursors.total, pageSize)
			continue
		}
		out = append(out, chunkPageCursors{pack: p, cursors: cursors})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no benchmarked chunk holds %d transactions (--page-size)", pageSize)
	}
	return out, nil
}

// coldTxPageOp returns the op timing one cold transactions-page read: open
// the pack's production reader at a drawn cursor and fetch+walk ledgers
// until the page is covered. The pack is evicted first, untimed.
func coldTxPageOp(cursors []chunkPageCursors, pageSize int) queryOp {
	return func(_ context.Context, rng *rand.Rand, record recordFn) error {
		cc := cursors[rng.IntN(len(cursors))]
		if err := evictFromPageCache(cc.pack.path); err != nil {
			return err
		}
		seq, skip := cc.cursors.draw(rng, pageSize)
		t := time.Now()
		cr, err := ledger.OpenColdReader(cc.pack.path)
		if err != nil {
			return err
		}
		defer func() { _ = cr.Close() }()
		if err := walkPage(cr, seq, skip, pageSize); err != nil {
			return err
		}
		record(rowTotal, time.Since(t), pageSize)
		return nil
	}
}

// coldLedgerRouter routes GetLedgerRaw by ledger sequence to the owning
// chunk's cold pack reader — the ledger seam TxReader needs when a lookup
// can resolve into any benchmarked chunk. Readers are opened up front
// (opening does no I/O) and shared: ColdReader reads are safe for
// concurrent use.
type coldLedgerRouter struct {
	readers map[chunk.ID]*ledger.ColdReader
	packs   map[chunk.ID]string
}

func newColdLedgerRouter(packs []coldPack) (*coldLedgerRouter, error) {
	r := &coldLedgerRouter{
		readers: make(map[chunk.ID]*ledger.ColdReader, len(packs)),
		packs:   make(map[chunk.ID]string, len(packs)),
	}
	for _, p := range packs {
		cr, err := ledger.OpenColdReader(p.path)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		r.readers[p.id] = cr
		r.packs[p.id] = p.path
	}
	return r, nil
}

func (r *coldLedgerRouter) GetLedgerRaw(seq uint32) ([]byte, error) {
	cr, ok := r.readers[chunk.IDFromLedger(seq)]
	if !ok {
		return nil, fmt.Errorf("ledger %d is outside the benchmarked chunks", seq)
	}
	return cr.GetLedgerRaw(seq)
}

func (r *coldLedgerRouter) Close() error {
	errs := make([]error, 0, len(r.readers))
	for _, cr := range r.readers {
		errs = append(errs, cr.Close())
	}
	return errors.Join(errs...)
}

// evict drops one benchmarked chunk's pack from the page cache.
func (r *coldLedgerRouter) evict(c chunk.ID) error {
	path, ok := r.packs[c]
	if !ok {
		return fmt.Errorf("chunk %d is not benchmarked", uint32(c))
	}
	return evictFromPageCache(path)
}

// resolveTxhashIndex returns the cold tx-hash MPHF path to benchmark
// against: the explicit --txhash-index when given, otherwise one built from
// the benchmarked chunks' .bin files into a temp dir (untimed setup,
// reported as the txhash_index_build driver row). cleanup removes the temp
// build, if any.
func resolveTxhashIndex(
	ctx context.Context,
	logger *supportlog.Entry,
	layout geometry.Layout,
	packs []coldPack,
	explicit string,
	sink *csvSink,
) (string, func(), error) {
	noop := func() {}
	if explicit != "" {
		return explicit, noop, nil
	}
	bins := make([]string, 0, len(packs))
	for _, p := range packs {
		bin := layout.TxHashBinPath(p.id)
		if _, err := os.Stat(bin); err != nil {
			return "", noop, fmt.Errorf(
				"txhash .bin for chunk %d missing (produce it with bench-ingest cold --types=txhash): %w",
				uint32(p.id), err)
		}
		bins = append(bins, bin)
	}
	tmpDir, err := os.MkdirTemp("", "bench-query-txhash-")
	if err != nil {
		return "", noop, fmt.Errorf("create txhash index scratch dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	// The name is cosmetic — every consumer takes this explicit path.
	// Production naming (geometry.TxHashIndexFilePath) needs a full index
	// coverage, which a bench scratch build doesn't have.
	idxPath := filepath.Join(tmpDir, packs[0].id.String()+"-txhash.idx")
	minLedger, maxLedger := packs[0].first, packs[len(packs)-1].last
	t := time.Now()
	if err := txhash.BuildColdIndex(ctx, bins, idxPath, minLedger, maxLedger); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("build txhash cold index: %w", err)
	}
	sink.observeDriver(driverIndexBuild, time.Since(t), len(packs))
	logger.Infof("built txhash cold index over %d chunks in %s (scratch: %s)",
		len(packs), time.Since(t).Round(time.Millisecond), idxPath)
	return idxPath, cleanup, nil
}

// coldTxhashSetup assembles the production cold GetTransaction path — MPHF
// index + per-chunk pack readers behind one TxReader — and samples the hash
// corpus from the benchmarked ledgers. The returned cleanup closes the
// readers and removes any temp index build.
func coldTxhashSetup(
	ctx context.Context,
	logger *supportlog.Entry,
	layout geometry.Layout,
	packs []coldPack,
	opts coldQueryOptions,
	sink *csvSink,
) (queryOp, func(), error) {
	router, err := newColdLedgerRouter(packs)
	if err != nil {
		return nil, nil, err
	}
	idxPath, cleanupIdx, err := resolveTxhashIndex(ctx, logger, layout, packs, opts.TxhashIndex, sink)
	if err != nil {
		_ = router.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = router.Close()
		cleanupIdx()
	}
	idx, err := txhash.OpenColdReader(idxPath)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open txhash cold index %s: %w", idxPath, err)
	}
	cleanup = func() {
		_ = idx.Close()
		_ = router.Close()
		cleanupIdx()
	}
	// Start the run with a cold index: a full-history index far exceeds RAM
	// and uniform-random lookups have no hot subset, so steady state keeps
	// only the pages lookups actually touch warm.
	if err := evictFromPageCache(idxPath); err != nil {
		cleanup()
		return nil, nil, err
	}
	txr, err := txhash.NewTxReader(nil, []txhash.HashIndex{idx}, router, opts.Passphrase)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	pool, err := sampleColdHashes(router, packs, opts, sink)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return txhashOp(txr, pool, router.evict), cleanup, nil
}

// sampleColdHashes builds the tx-hash corpus from the benchmarked packs.
func sampleColdHashes(
	router *coldLedgerRouter, packs []coldPack, opts coldQueryOptions, sink *csvSink,
) (*hashPool, error) {
	pool := &hashPool{}
	rng := opts.corpusRNG()
	t := time.Now()
	for _, p := range packs {
		if err := samplePoolHashes(router, p.first, p.last, opts.SampleLedgers, rng, pool); err != nil {
			return nil, err
		}
	}
	if len(pool.hashes) == 0 {
		return nil, errors.New("txhash corpus: no transactions in the sampled ledgers; raise --sample-ledgers")
	}
	sink.observeDriver(driverHashSample, time.Since(t), len(pool.hashes))
	return pool, nil
}

// chunkEventsCorpus is one chunk's events artifacts plus the query corpus
// derived from them.
type chunkEventsCorpus struct {
	id        chunk.ID
	bucketDir string
	paths     []string
	corpus    *eventCorpus
}

// coldEventsSetup derives each benchmarked chunk's query corpus by scanning
// its events through a temporary production reader (untimed, reported as
// the events_corpus_scan driver row).
func coldEventsSetup(
	ctx context.Context, layout geometry.Layout, chunks []chunk.ID, opts coldQueryOptions, sink *csvSink,
) ([]chunkEventsCorpus, error) {
	out := make([]chunkEventsCorpus, 0, len(chunks))
	for _, c := range chunks {
		dir := layout.EventsBucketDir(c)
		r, err := eventstore.OpenColdReader(c, dir, eventstore.ColdReaderOptions{Concurrency: opts.ReaderConcurrency})
		if err != nil {
			return nil, fmt.Errorf("open events chunk %d: %w", uint32(c), err)
		}
		t := time.Now()
		corpus, cerr := buildEventCorpus(ctx, r)
		if err := errors.Join(cerr, r.Close()); err != nil {
			return nil, fmt.Errorf("events chunk %d: %w", uint32(c), err)
		}
		sink.observeDriver(driverCorpusScan, time.Since(t), len(corpus.terms))
		out = append(out, chunkEventsCorpus{id: c, bucketDir: dir, paths: layout.EventsPaths(c), corpus: corpus})
	}
	return out, nil
}

// coldEventsOp returns the op timing one cold events query end-to-end: open
// the chunk's production reader fresh (index load included — that is part
// of serving a cold chunk) and run the full Query, MPHF probe and
// post-filter included. The chunk's three event files are evicted first,
// untimed.
func coldEventsOp(corpora []chunkEventsCorpus, maxEvents, readerConcurrency int) queryOp {
	return func(ctx context.Context, rng *rand.Rand, record recordFn) error {
		ec := corpora[rng.IntN(len(corpora))]
		for _, p := range ec.paths {
			if err := evictFromPageCache(p); err != nil {
				return err
			}
		}
		filters := ec.corpus.draw(rng)
		t := time.Now()
		r, err := eventstore.OpenColdReader(ec.id, ec.bucketDir,
			eventstore.ColdReaderOptions{Concurrency: readerConcurrency})
		if err != nil {
			return fmt.Errorf("open events chunk %d: %w", uint32(ec.id), err)
		}
		n, qerr := runEventsQuery(ctx, r, filters, maxEvents)
		d := time.Since(t)
		if err := errors.Join(qerr, r.Close()); err != nil {
			return fmt.Errorf("events chunk %d: %w", uint32(ec.id), err)
		}
		record(rowTotal, d, n)
		return nil
	}
}
