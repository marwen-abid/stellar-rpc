package bench

import (
	"fmt"
	"os"
	"time"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
)

// queryPlan is the validated run: which types, at which arrival rates, how long
// each leg runs, and the shape of each query.
type queryPlan struct {
	Types     []string
	TargetRPS []float64
	Duration  time.Duration
	Warmup    int

	LedgersSpan  uint32
	TxPageSpan   uint32
	TxPageLimit  int
	EventsLimit  int
	MissFraction float64
	Passphrase   string
	Seed         int64

	// Evict drops the cold artifacts from the OS page cache before each leg's
	// measured requests. Cold only: a hot leg warms the cache instead.
	Evict bool
}

// queryFixture is the read side of one bench-query run over a dataset a
// bench-ingest run left on disk: the scratch catalog that makes it servable,
// the registry holding the hot handles, and the ledger range the corpora may
// sample from.
//
// Every query takes a read view and resolves its tier through ReadView, as a
// served request does. The cold fixture publishes no hot handle and the hot one
// freezes no artifact, so a resolve that succeeds proves the intended tier
// served it.
type queryFixture struct {
	registry *query.Registry

	// Passphrase is the network the dataset's transactions were signed under.
	// txpage and txhash pair envelopes with it; a wrong one makes every lookup a miss.
	Passphrase string

	// Chunks is the benchmarked chunk range, ascending.
	Chunks []chunk.ID

	// FirstLedger and LastLedger bound the ledgers the corpora may sample: the
	// chunk range for a cold fixture, what the database holds for a hot one.
	FirstLedger, LastLedger uint32

	// EvictPaths are the on-disk artifacts a cold leg drops from the page cache.
	// Empty for a hot fixture: RocksDB's caches cannot be advised from here.
	EvictPaths []string
}

// view acquires one read view. Every measured query takes its own, as a served
// request does; the caller MUST Release it.
func (f *queryFixture) view() (*query.ReadView, error) {
	return f.registry.NewReadView()
}

// verifyServes resolves each benchmarked chunk's ledger store through a read
// view, so a dataset that cannot be served fails at open with the chunk named.
func (f *queryFixture) verifyServes() error {
	view, err := f.view()
	if err != nil {
		return fmt.Errorf("acquire read view: %w", err)
	}
	defer view.Release()
	for _, c := range f.Chunks {
		if _, err := view.Ledgers(c); err != nil {
			return fmt.Errorf("chunk %s has no servable ledger store: %w", c, err)
		}
	}
	return nil
}

// evictColdArtifacts drops the fixture's cold artifacts from the OS page cache
// before a leg and reports how many files it advised. A missing file is
// skipped: the list is derived from the layout, so a kind the dataset never
// produced is a legitimate absence.
func (f *queryFixture) evictColdArtifacts() (int, error) {
	evicted := 0
	for _, path := range f.EvictPaths {
		if err := evictFile(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return evicted, fmt.Errorf("evict %s from the page cache: %w", path, err)
		}
		evicted++
	}
	return evicted, nil
}

// chunkRange returns the ascending chunk IDs in [start, start+num). The caller's
// validate() proved start+num-1 stays within maxChunkID.
func chunkRange(start chunk.ID, num int) []chunk.ID {
	chunks := make([]chunk.ID, 0, num)
	for i := range uint32(num) { //nolint:gosec // num >= 1, bounded by validate()
		chunks = append(chunks, start+chunk.ID(i))
	}
	return chunks
}
