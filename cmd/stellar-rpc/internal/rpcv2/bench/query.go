package bench

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
)

// NewQueryCommand returns the `bench-query` command tree: `cold` benchmarks
// reads served from frozen artifacts, `hot` reads served from a hot chunk
// database. Both read through query.ReadView.
func NewQueryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench-query",
		Short: "Benchmark full-history reads",
	}
	cmd.AddCommand(newQueryColdCommand(), newQueryHotCommand())
	return cmd
}

// Read-shape flag defaults. The ledgers span and the events limit are the SLA
// request shapes (getLedgers max=10, getEvents 10); the txpage span and limit
// are the v2 page caps; the miss fraction is the production share of by-hash
// lookups for a hash that never landed.
const (
	defaultLedgersSpan  = 10
	defaultTxPageSpan   = 5
	defaultTxPageLimit  = 200
	defaultEventsLimit  = 10
	defaultMissFraction = 0.12
	defaultSeed         = 1
)

// Leg flag defaults.
const (
	defaultLegDuration = 60 * time.Second
	defaultTargetRPS   = "10"
)

// maxTargetRPS is the highest arrival rate --target-rps accepts.
const maxTargetRPS = 1_000_000

// maxReadSpan is the widest span --ledgers-span and --txpage-span accept. A
// request's last ledger is start+span-1 in uint32 and must not wrap.
const maxReadSpan = chunk.LedgersPerChunk

// queryFlags is the flag set both bench-query subcommands share, beyond --out
// and the profiling flags newBenchCommand binds. The spellings and value
// formats of --types, --target-rps, --duration and --warmup are the campaign
// runner's argv contract.
type queryFlags struct {
	types       string
	targetRPS   string
	duration    time.Duration
	warmup      int
	warmupBound bool // bind --warmup (hot only)

	ledgersSpan  uint32
	txPageSpan   uint32
	txPageLimit  int
	eventsLimit  int
	missFraction float64
	passphrase   string
	seed         int64
}

func (f *queryFlags) bind(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&f.types, "types", strings.Join(allQueryTypes, ","),
		"comma-separated query types to run: "+strings.Join(allQueryTypes, " | "))
	fs.StringVar(&f.targetRPS, "target-rps", defaultTargetRPS,
		"comma-separated arrival rates to run, in requests per second, e.g. 0.5,1,2")
	fs.DurationVar(&f.duration, "duration", defaultLegDuration, "how long each --target-rps leg runs")
	if f.warmupBound {
		fs.IntVar(&f.warmup, "warmup", f.warmup,
			"unmeasured queries per leg, dispatched at the leg's rate before measurement starts, "+
				"warming the store's caches")
	}
	fs.Uint32Var(&f.ledgersSpan, "ledgers-span", defaultLedgersSpan,
		"ledgers one ledgers query scans (1 = a point read)")
	fs.Uint32Var(&f.txPageSpan, "txpage-span", defaultTxPageSpan,
		"ledgers one txpage query walks")
	fs.IntVar(&f.txPageLimit, "txpage-limit", defaultTxPageLimit,
		"transactions one txpage query materializes before it stops, as a page cap")
	fs.IntVar(&f.eventsLimit, "events-limit", defaultEventsLimit,
		"events one events page may return")
	fs.Float64Var(&f.missFraction, "miss-fraction", defaultMissFraction,
		"share of txhash lookups asking for a hash that never landed, in [0, 1] "+
			"(a miss probes every index, so it is the path's worst case)")
	fs.StringVar(&f.passphrase, "network-passphrase", network.PublicNetworkPassphrase,
		"network passphrase the dataset's transactions were signed under; txhash and "+
			"txpage need it to pair envelopes, and a wrong one fails the corpus build")
	fs.Int64Var(&f.seed, "seed", defaultSeed,
		"seed for the work each query picks, so a re-run reads the same ledgers")
}

// plan parses and validates the flags.
func (f *queryFlags) plan() (queryPlan, error) {
	types, err := parseQueryTypes(f.types)
	if err != nil {
		return queryPlan{}, err
	}
	rates, err := parseTargetRPS(f.targetRPS)
	if err != nil {
		return queryPlan{}, err
	}
	switch {
	case f.duration <= 0:
		return queryPlan{}, fmt.Errorf("--duration must be > 0, got %v", f.duration)
	case f.warmup < 0:
		return queryPlan{}, fmt.Errorf("--warmup must be >= 0, got %d", f.warmup)
	case f.ledgersSpan < 1 || f.ledgersSpan > maxReadSpan:
		return queryPlan{}, fmt.Errorf("--ledgers-span must be in [1, %d], got %d", maxReadSpan, f.ledgersSpan)
	case f.txPageSpan < 1 || f.txPageSpan > maxReadSpan:
		return queryPlan{}, fmt.Errorf("--txpage-span must be in [1, %d], got %d", maxReadSpan, f.txPageSpan)
	case f.txPageLimit < 1:
		return queryPlan{}, fmt.Errorf("--txpage-limit must be >= 1, got %d", f.txPageLimit)
	case f.eventsLimit < 1:
		return queryPlan{}, fmt.Errorf("--events-limit must be >= 1, got %d", f.eventsLimit)
	case f.missFraction < 0 || f.missFraction > 1:
		return queryPlan{}, fmt.Errorf("--miss-fraction must be in [0, 1], got %v", f.missFraction)
	case f.passphrase == "":
		return queryPlan{}, errors.New("--network-passphrase is required")
	}
	return queryPlan{
		Types:        types,
		TargetRPS:    rates,
		Duration:     f.duration,
		Warmup:       f.warmup,
		LedgersSpan:  f.ledgersSpan,
		TxPageSpan:   f.txPageSpan,
		TxPageLimit:  f.txPageLimit,
		EventsLimit:  f.eventsLimit,
		MissFraction: f.missFraction,
		Passphrase:   f.passphrase,
		Seed:         f.seed,
	}, nil
}

// parseQueryTypes splits --types, in the caller's order. A type names its own
// CSV file, so a repeat is an error.
func parseQueryTypes(s string) ([]string, error) {
	fields := strings.Split(s, ",")
	types := make([]string, 0, len(fields))
	for _, f := range fields {
		qtype := strings.TrimSpace(f)
		if qtype == "" {
			return nil, fmt.Errorf("--types has an empty entry: %q", s)
		}
		if !slices.Contains(allQueryTypes, qtype) {
			return nil, fmt.Errorf("--types: unknown query type %q (want %s)",
				qtype, strings.Join(allQueryTypes, " | "))
		}
		if slices.Contains(types, qtype) {
			return nil, fmt.Errorf("--types repeats %q", qtype)
		}
		types = append(types, qtype)
	}
	return types, nil
}

// parseTargetRPS splits --target-rps into arrival rates, in the caller's order.
// A rate names its leg's CSV rows, so a repeat is an error.
func parseTargetRPS(s string) ([]float64, error) {
	fields := strings.Split(s, ",")
	rates := make([]float64, 0, len(fields))
	for _, f := range fields {
		field := strings.TrimSpace(f)
		if field == "" {
			return nil, fmt.Errorf("--target-rps has an empty entry: %q", s)
		}
		rps, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, fmt.Errorf("--target-rps: %q is not a list of numbers", s)
		}
		if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return nil, fmt.Errorf("--target-rps rates must be > 0, got %v", rps)
		}
		if rps > maxTargetRPS {
			return nil, fmt.Errorf("--target-rps rates must be <= %v, got %v", float64(maxTargetRPS), rps)
		}
		if slices.Contains(rates, rps) {
			return nil, fmt.Errorf("--target-rps repeats %v", rps)
		}
		rates = append(rates, rps)
	}
	return rates, nil
}
