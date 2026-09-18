package bench

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
)

// queryPlan is the validated run.
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

	// TxHashCorpusSize caps the sampled pool. It is always in
	// [1, maxTxHashCorpusSize].
	TxHashCorpusSize int

	// Evict requests OS page-cache eviction before each cold leg.
	Evict bool

	// Extra receives invocation metadata. It may be nil for internal callers.
	Extra map[string]string
}

func (p queryPlan) cacheScenario() string {
	switch {
	case p.Warmup > 0:
		return "warm-run"
	case p.Evict:
		return "cold-start"
	default:
		return "existing-cache"
	}
}

// queryFixture is the read side of one bench-query run: the registry over a
// dataset a bench-ingest run left on disk, and the ledger range the corpora may
// sample. Every query takes a read view and resolves its tier through
// ReadView. The cold fixture publishes no hot handle; the hot one freezes no
// artifact.
type queryFixture struct {
	registry *query.Registry

	// Passphrase is the network the dataset's transactions were signed under.
	Passphrase string

	// Chunks is the benchmarked chunk range, ascending.
	Chunks []chunk.ID

	// FirstLedger and LastLedger bound the ledgers the corpora may sample.
	FirstLedger, LastLedger uint32

	// EvictPaths are the files a cold leg requests page-cache eviction for. Empty for
	// a hot fixture.
	EvictPaths []string
}

// view acquires one read view. The caller must Release it.
func (f *queryFixture) view() (*query.ReadView, error) {
	return f.registry.NewReadView()
}

// verifyServes resolves the stores the requested types read from, one read view
// per chunk. Every type reads ledgers; events also needs an events store.
func (f *queryFixture) verifyServes(types []string) error {
	events := slices.Contains(types, queryTypeEvents)
	for _, c := range f.Chunks {
		view, err := f.view()
		if err != nil {
			return fmt.Errorf("acquire read view: %w", err)
		}
		_, err = view.Ledgers(c)
		if err != nil {
			view.Release()
			return fmt.Errorf("chunk %s has no servable ledger store: %w", c, err)
		}
		if events {
			_, err = view.Events(c)
		}
		view.Release()
		if err != nil {
			return fmt.Errorf("chunk %s has no servable events store: %w", c, err)
		}
	}
	return nil
}

// evictColdArtifacts requests page-cache eviction and counts successful calls.
// A missing file is skipped. Off Linux nothing can be evicted and the count is
// zero; invocation metadata records that eviction is unsupported.
func (f *queryFixture) evictColdArtifacts() (int, error) {
	if !evictSupported {
		return 0, nil
	}
	evicted := 0
	for _, path := range f.EvictPaths {
		if err := evictFile(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return evicted, fmt.Errorf("evict %s from the page cache: %w", path, err)
		}
		evicted++
	}
	return evicted, nil
}

// chunkRange returns the ascending chunk IDs in [start, start+num). The caller
// validated start+num-1 <= maxChunkID.
func chunkRange(start chunk.ID, num int) []chunk.ID {
	chunks := make([]chunk.ID, 0, num)
	for i := range uint32(num) { //nolint:gosec // num >= 1, bounded by validate()
		chunks = append(chunks, start+chunk.ID(i))
	}
	return chunks
}
