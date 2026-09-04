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
)

// NewQueryCommand returns the `bench-query` command tree: `cold` benchmarks
// reads served from frozen artifacts, `hot` reads served from a hot chunk
// database. Both measure through query.ReadView, the stable read seam, so a
// store-reader refactor moves the numbers without moving the benchmark.
func NewQueryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench-query",
		Short: "Benchmark full-history reads",
	}
	cmd.AddCommand(newQueryColdCommand(), newQueryHotCommand())
	return cmd
}

// Defaults for the read-shape flags. The ledgers span and the events limit are
// the request shapes the team SLA fixes (getLedgers max=10; getEvents 10
// matches), so a default run measures the shapes the SLA rows quote; the txpage
// span and limit are the v2 page caps; the miss fraction matches the share of
// by-hash lookups a production node sees for a hash that never landed.
const (
	defaultLedgersSpan  = 10
	defaultTxPageSpan   = 5
	defaultTxPageLimit  = 200
	defaultEventsLimit  = 10
	defaultMissFraction = 0.12
	defaultSeed         = 1
)

// Defaults for the two flags that shape a leg: long enough that a slow rate
// schedules a useful number of requests, short enough that a four-type ladder
// finishes in minutes; one modest rate, so a bare invocation runs one leg per type.
const (
	defaultLegDuration = 60 * time.Second
	defaultTargetRPS   = "10"
)

// maxTargetRPS is the highest arrival rate --target-rps accepts: above it the
// single dispatch loop's own speed is what gets measured.
const maxTargetRPS = 1_000_000

// queryFlags is the flag set both bench-query subcommands share, beyond --out
// and the profiling flags newBenchCommand binds. The spellings and value formats
// of --types, --target-rps, --duration and --warmup are the campaign runner's
// argv contract; the read-shape flags are bench-side only.
type queryFlags struct {
	types       string
	targetRPS   string
	duration    time.Duration
	warmup      int
	warmupBound bool // bind --warmup (hot only; a cold leg evicts its page cache)

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
	case f.ledgersSpan < 1:
		return queryPlan{}, fmt.Errorf("--ledgers-span must be >= 1, got %d", f.ledgersSpan)
	case f.txPageSpan < 1:
		return queryPlan{}, fmt.Errorf("--txpage-span must be >= 1, got %d", f.txPageSpan)
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

// parseQueryTypes splits --types, keeping the caller's order and rejecting an
// empty list, an unknown type, or a repeat (a type names its own CSV file).
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

// parseTargetRPS splits --target-rps into the arrival rates to run, keeping the
// caller's order and rejecting an empty entry, a non-number, a rate that is not
// a positive finite number, a rate above maxTargetRPS, or a repeat (a rate
// names its leg's CSV rows).
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
