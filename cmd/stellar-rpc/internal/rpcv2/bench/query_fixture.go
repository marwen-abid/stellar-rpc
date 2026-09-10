package bench

import (
	"errors"
	"fmt"
	"io/fs"
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

	// TxHashCorpusSize caps the sampled pool. Zero uses corpusTargetHashes.
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

// verifyServes resolves each chunk's ledger store through a read view.
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

// evictColdArtifacts requests page-cache eviction and counts successful calls.
// Linux skips missing files. Off Linux, calls are no-ops that count every path;
// invocation metadata records that eviction is unsupported.
func (f *queryFixture) evictColdArtifacts() (int, error) {
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
