package bench

import (
	"fmt"
	"os"
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

	// Evict drops the cold artifacts from the OS page cache before each leg.
	// Cold only.
	Evict bool
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

	// EvictPaths are the files a cold leg drops from the page cache. Empty for
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

// evictColdArtifacts drops EvictPaths from the OS page cache and returns how
// many files it advised. A missing file is skipped.
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

// chunkRange returns the ascending chunk IDs in [start, start+num). The caller
// validated start+num-1 <= maxChunkID.
func chunkRange(start chunk.ID, num int) []chunk.ID {
	chunks := make([]chunk.ID, 0, num)
	for i := range uint32(num) { //nolint:gosec // num >= 1, bounded by validate()
		chunks = append(chunks, start+chunk.ID(i))
	}
	return chunks
}
