package bench

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/hotchunk"
)

// defaultHotWarmup is --warmup's default.
const defaultHotWarmup = 20

func newQueryHotCommand() *cobra.Command {
	var (
		qf   = queryFlags{warmup: defaultHotWarmup}
		prof profileFlags

		chunkID       uint32
		hotDir        string
		sampleLedgers uint32
	)
	cmd := newBenchCommand("hot",
		"Benchmark hot reads: queries served from one chunk's hot database",
		&prof,
		func(ctx context.Context, logger *supportlog.Entry, env runEnv) error {
			plan, err := qf.plan()
			if err != nil {
				return err
			}
			plan.Extra = env.Extra
			env.Extra["pageCacheEviction"] = evictionState(false)
			env.Extra["cacheScenario"] = plan.cacheScenario()
			return runQueryHot(ctx, logger, hotQueryOptions{
				HotRoot:       hotDir,
				Chunk:         chunk.ID(chunkID),
				SampleLedgers: sampleLedgers,
				Plan:          plan,
				OutDir:        env.OutDir,
			})
		}, &qf)
	fs := cmd.Flags()
	fs.Uint32Var(&chunkID, "chunk", 0, "the chunk to query (required)")
	fs.StringVar(&hotDir, "hot-dir", "",
		"root holding the hot chunk databases, as bench-ingest hot's --hot-dir laid it out (required)")
	fs.Uint32Var(&sampleLedgers, "sample-ledgers", 0,
		"cap the sampled ledgers to this many from the chunk's start (0 = every ledger the database holds); "+
			"match a capped ingest so the corpora stay inside what was ingested")
	markRequired(cmd, "chunk", "hot-dir")
	return cmd
}

// hotQueryOptions configures one hot read benchmark run.
type hotQueryOptions struct {
	// HotRoot is the layout root of the hot chunk databases. The database is
	// opened read-write.
	HotRoot string

	// Chunk is the chunk whose hot database is queried.
	Chunk chunk.ID

	// SampleLedgers caps the sampled ledgers to this many from the chunk's
	// first; 0 = every ledger the database holds.
	SampleLedgers uint32

	// Plan is the validated flags.
	Plan queryPlan

	// OutDir receives the CSV report.
	OutDir string
}

// validate checks the flags.
func (o hotQueryOptions) validate() error {
	if o.HotRoot == "" {
		return errors.New("--hot-dir is required")
	}
	if o.Chunk > maxChunkID {
		return fmt.Errorf("--chunk=%d is past the last valid chunk ID %d", uint32(o.Chunk), uint32(maxChunkID))
	}
	return nil
}

// runQueryHot benchmarks the hot read path: queries against one chunk's hot
// database under --hot-dir.
func runQueryHot(ctx context.Context, logger *supportlog.Entry, opts hotQueryOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	return runQueryBench(ctx, logger, opts.Plan, opts.OutDir, func() (*queryFixture, func(), error) {
		return openHotFixture(logger, opts)
	})
}

// openHotFixture opens one chunk's hot database and returns the read fixture
// over it, plus its release.
//
// The database has no catalog: bench-ingest hot discards its scratch catalog.
// The chunk's hot key runs the ready bracket and the database is opened with
// OpenReadyWrite, the must-exist open; query.OpenRegistry is the daemon's own
// startup sequence. Nothing is frozen, so only the hot tier can serve. The
// latest ledger is MaxCommittedSeq, not the chunk's nominal last: a capped
// ingest stops mid-chunk. --sample-ledgers narrows the sampled range, clamped
// to what was ingested.
func openHotFixture(logger *supportlog.Entry, opts hotQueryOptions) (*queryFixture, func(), error) {
	layout := geometry.NewLayout(opts.HotRoot)
	cat, releaseCat, err := openScratchCatalog(opts.HotRoot, scratchPrefixQuery, layout, logger)
	if err != nil {
		return nil, nil, err
	}
	if err := cat.PutHotTransient(opts.Chunk); err != nil {
		releaseCat()
		return nil, nil, fmt.Errorf("mark hot chunk %s transient: %w", opts.Chunk, err)
	}
	if err := cat.FlipHotReady(opts.Chunk); err != nil {
		releaseCat()
		return nil, nil, fmt.Errorf("mark hot chunk %s ready: %w", opts.Chunk, err)
	}

	path := layout.HotChunkPath(opts.Chunk)
	db, err := hotchunk.OpenReadyWrite(geometry.HotReady, path, opts.Chunk, logger)
	if err != nil {
		releaseCat()
		return nil, nil, fmt.Errorf("open hot chunk %s at %s: %w", opts.Chunk, path, err)
	}

	committed, ok, err := db.MaxCommittedSeq()
	if err != nil {
		_ = db.Close()
		releaseCat()
		return nil, nil, fmt.Errorf("read hot chunk %s last committed ledger: %w", opts.Chunk, err)
	}
	if !ok {
		_ = db.Close()
		releaseCat()
		return nil, nil, fmt.Errorf("hot chunk %s holds no committed ledger: ingest it before querying it", opts.Chunk)
	}

	registry, err := query.OpenRegistry(cat, geometry.NewRetention(0, opts.Chunk), db, committed)
	if err != nil {
		_ = db.Close()
		releaseCat()
		return nil, nil, fmt.Errorf("open the read registry over hot chunk %s: %w", opts.Chunk, err)
	}
	// Registry.Close closes every published handle, db included.
	release := func() {
		registry.Close()
		releaseCat()
	}

	first := opts.Chunk.FirstLedger()
	last := committed
	// Compare spans: first+SampleLedgers can wrap.
	if span := committed - first + 1; opts.SampleLedgers > 0 && opts.SampleLedgers < span {
		last = first + opts.SampleLedgers - 1
	}
	f := &queryFixture{
		registry:    registry,
		Passphrase:  opts.Plan.Passphrase,
		Chunks:      []chunk.ID{opts.Chunk},
		FirstLedger: first,
		LastLedger:  last,
	}
	if err := f.verifyServes(); err != nil {
		release()
		return nil, nil, err
	}
	return f, release, nil
}
