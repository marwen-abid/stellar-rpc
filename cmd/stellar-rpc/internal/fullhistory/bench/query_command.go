package bench

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
)

// NewQueryCommand returns the `bench-query` command tree: `cold` benchmarks
// the production cold readers (ledger packfiles, the tx-hash MPHF index, the
// events index + post-filter), `hot` the production hot RocksDB read paths —
// both sweeping --query-concurrency and reporting per-cell percentile CSVs
// through the same sink and schema as bench-ingest.
func NewQueryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench-query",
		Short: "Benchmark full-history queries against the production read paths",
	}
	cmd.AddCommand(newQueryColdCommand(), newQueryHotCommand())
	return cmd
}

// queryFlags is the workload flag set shared by both query subcommands.
type queryFlags struct {
	types            string
	queryConcurrency string
	iters            int
	seed             uint64
	ledgersPerRead   int
	pageSize         int
	maxEvents        int
	sampleLedgers    int
	passphrase       string
	out              string
}

func (f *queryFlags) bind(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&f.types, "types", "",
		"comma-separated subset of ledgers,txpage,txhash,events (required)")
	fs.StringVar(&f.queryConcurrency, "query-concurrency", "1",
		"comma-separated worker counts to sweep, e.g. 1,4,16")
	fs.IntVar(&f.iters, "iters", 100, "measured iterations per worker per sweep cell")
	fs.Uint64Var(&f.seed, "seed", 1, "PRNG seed for query-input selection (same seed = same query sequence)")
	fs.IntVar(&f.ledgersPerRead, "ledgers-per-read", 20, "consecutive ledgers per ledgers-bench read")
	fs.IntVar(&f.pageSize, "page-size", 20, "transactions per txpage-bench page")
	fs.IntVar(&f.maxEvents, "max-events", 1000, "events-bench per-query result cap (0 = uncapped)")
	fs.IntVar(&f.sampleLedgers, "sample-ledgers", 100,
		"ledgers sampled per chunk for the txhash query corpus")
	fs.StringVar(&f.passphrase, "network-passphrase", network.PublicNetworkPassphrase,
		"network passphrase for transaction-hash verification")
	fs.StringVar(&f.out, "out", "bench-out", "CSV output dir")
}

// knobs resolves the flag values into validated-shape workload knobs (full
// validation happens in the drivers' validate).
func (f *queryFlags) knobs() (queryKnobs, error) {
	types, err := parseQueryTypes(f.types)
	if err != nil {
		return queryKnobs{}, err
	}
	workers, err := parseWorkersList(f.queryConcurrency)
	if err != nil {
		return queryKnobs{}, err
	}
	return queryKnobs{
		Types:          types,
		Workers:        workers,
		Iters:          f.iters,
		Seed:           f.seed,
		LedgersPerRead: f.ledgersPerRead,
		PageSize:       f.pageSize,
		MaxEvents:      f.maxEvents,
		SampleLedgers:  f.sampleLedgers,
		Passphrase:     f.passphrase,
		OutDir:         f.out,
	}, nil
}

// parseQueryTypes turns the --types flag value into a queryTypes selection.
func parseQueryTypes(arg string) (queryTypes, error) {
	var types queryTypes
	for t := range strings.SplitSeq(arg, ",") {
		switch strings.TrimSpace(t) {
		case "ledgers":
			types.Ledgers = true
		case typeTxPage:
			types.TxPage = true
		case "txhash":
			types.Txhash = true
		case "events":
			types.Events = true
		case "":
		default:
			return types, fmt.Errorf(
				"--types: unknown data type %q (expected subset of ledgers,txpage,txhash,events)", t)
		}
	}
	return types, nil
}

// parseWorkersList turns the --query-concurrency flag value into the swept
// worker counts.
func parseWorkersList(arg string) ([]int, error) {
	var out []int
	for s := range strings.SplitSeq(arg, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		w, err := strconv.Atoi(s)
		if err != nil || w < 1 {
			return nil, fmt.Errorf("--query-concurrency: %q is not a positive worker count", s)
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, errors.New("--query-concurrency must list at least one worker count")
	}
	return out, nil
}

func newQueryColdCommand() *cobra.Command {
	var (
		flags             queryFlags
		coldDir           string
		chunkArg          uint32
		numChunks         int
		txhashIndex       string
		readerConcurrency int
		prof              profileFlags
	)
	cmd := &cobra.Command{
		Use:   "cold",
		Short: "Benchmark cold queries (packfiles + MPHF indexes) over frozen chunk artifacts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			knobs, err := flags.knobs()
			if err != nil {
				return err
			}
			ctx, stop, logger := benchContext()
			defer stop()
			return prof.around(logger, func() error {
				return runQueryCold(ctx, logger, cmd, coldQueryOptions{
					queryKnobs:        knobs,
					ColdRoot:          coldDir,
					StartChunk:        chunk.ID(chunkArg),
					NumChunks:         numChunks,
					TxhashIndex:       txhashIndex,
					ReaderConcurrency: readerConcurrency,
				})
			})
		},
	}
	flags.bind(cmd)
	prof.bind(cmd)
	fs := cmd.Flags()
	fs.StringVar(&coldDir, "cold-dir", "",
		"cold artifact tree to read — a bench-ingest run's --cold-out-dir (required)")
	fs.Uint32Var(&chunkArg, "start-chunk", 0, "chunk ID to start benchmarking from (required)")
	fs.IntVar(&numChunks, "num-chunks", 1, "how many consecutive chunks queries draw from starting at --chunk")
	fs.StringVar(&txhashIndex, "txhash-index", "",
		"existing cold tx-hash MPHF (.idx) to use; empty = build one from the chunks' .bin files (untimed)")
	fs.IntVar(&readerConcurrency, "reader-concurrency", 8, "cold events reader's packfile read fan-out")
	markRequired(cmd, "types", "start-chunk", "cold-dir")
	return cmd
}

func newQueryHotCommand() *cobra.Command {
	var (
		flags    queryFlags
		hotDir   string
		chunkArg uint32
		warmup   int
		prof     profileFlags
	)
	cmd := &cobra.Command{
		Use:   "hot",
		Short: "Benchmark hot queries against one chunk's hot RocksDB (the live serving handle)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			knobs, err := flags.knobs()
			if err != nil {
				return err
			}
			knobs.Warmup = warmup
			ctx, stop, logger := benchContext()
			defer stop()
			return prof.around(logger, func() error {
				return runQueryHot(ctx, logger, cmd, hotQueryOptions{
					queryKnobs: knobs,
					HotRoot:    hotDir,
					Chunk:      chunk.ID(chunkArg),
				})
			})
		},
	}
	flags.bind(cmd)
	prof.bind(cmd)
	fs := cmd.Flags()
	fs.StringVar(&hotDir, "hot-dir",
		"", "root the hot RocksDB lives under — a bench-ingest hot run's --hot-dir (required)")
	fs.Uint32Var(&chunkArg, "chunk", 0, "chunk ID whose hot DB is benchmarked (required)")
	fs.IntVar(&warmup, "warmup", 20, "untimed iterations per worker before each measured sweep cell")
	markRequired(cmd, "types", "chunk", "hot-dir")
	return cmd
}
