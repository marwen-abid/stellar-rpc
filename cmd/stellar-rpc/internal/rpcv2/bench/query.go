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

// Read-shape flag defaults.
const (
	defaultLedgersSpan      = 10
	defaultTxPageSpan       = 5
	defaultTxPageLimit      = 200
	defaultEventsLimit      = 10
	defaultNotFoundFraction = 0.12
	defaultSeed             = 1
)

// Scenario flag defaults.
const (
	defaultScenarioDuration = 60 * time.Second
	defaultTargetRPS        = "10"
)

// maxTargetRPS is the highest rate --target-rps accepts.
const maxTargetRPS = 100_000

// maxTxHashPoolSize is the largest --txhash-pool-size.
const maxTxHashPoolSize = 1_000_000

// The tier names bench-query registers its subcommands under.
const (
	queryTierCold = "cold"
	queryTierHot  = "hot"
)

// maxReadSpan is the widest span --ledgers-span and --txpage-span accept. A
// request's last ledger is start+span-1 in uint32 and must not wrap.
const maxReadSpan = chunk.LedgersPerChunk

// queryFlags is the flag set both bench-query subcommands share, beyond --out
// and the profiling flags newBenchCommand binds. The spellings and value
// formats of --types, --target-rps, --duration and --warmup are the campaign
// runner's argv contract.
type queryFlags struct {
	types     string
	targetRPS string
	duration  time.Duration
	warmup    int

	ledgersSpan      uint32
	txPageSpan       uint32
	txPageLimit      int
	eventsLimit      int
	notFoundFraction float64
	txHashPoolSize   int
	passphrase       string
	seed             int64
}

func (f *queryFlags) bind(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&f.types, "types", strings.Join(allQueryTypes, ","),
		"comma-separated query types to run: "+strings.Join(allQueryTypes, " | "))
	fs.StringVar(&f.targetRPS, "target-rps", defaultTargetRPS,
		"comma-separated target rates, in requests per second, at most "+
			strconv.Itoa(maxTargetRPS)+", e.g. 0.5,1,2")
	fs.DurationVar(&f.duration, "duration", defaultScenarioDuration, "how long each --target-rps scenario runs")
	fs.IntVar(&f.warmup, "warmup", f.warmup,
		"unmeasured iterations per scenario, started at the scenario's rate before measurement starts")
	fs.Uint32Var(&f.ledgersSpan, "ledgers-span", defaultLedgersSpan,
		"ledgers one ledgers query scans (1 = a point read)")
	fs.Uint32Var(&f.txPageSpan, "txpage-span", defaultTxPageSpan,
		"ledgers one txpage query walks")
	fs.IntVar(&f.txPageLimit, "txpage-limit", defaultTxPageLimit,
		"transactions one txpage query materializes before it stops, as a page cap")
	fs.IntVar(&f.eventsLimit, "events-limit", defaultEventsLimit,
		"events one events page may return")
	fs.Float64Var(&f.notFoundFraction, "not-found-fraction", defaultNotFoundFraction,
		"share of txhash lookups asking for a hash that never landed, in [0, 1] "+
			"(a not-found lookup probes every index, so it is the path's worst case)")
	fs.IntVar(&f.txHashPoolSize, "txhash-pool-size", poolTargetHashes,
		"best-effort txhash pool size, in [1, 1000000]; sampling may return fewer hashes")
	fs.StringVar(&f.passphrase, "network-passphrase", network.PublicNetworkPassphrase,
		"network passphrase the dataset's transactions were signed under; txhash and "+
			"txpage need it to pair envelopes, and a wrong one fails the pool build")
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
	if err := validateTxHashPoolSize(f.txHashPoolSize); err != nil {
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
	case !(f.notFoundFraction >= 0 && f.notFoundFraction <= 1): // also rejects NaN
		return queryPlan{}, fmt.Errorf("--not-found-fraction must be in [0, 1], got %v", f.notFoundFraction)
	case f.passphrase == "":
		return queryPlan{}, errors.New("--network-passphrase is required")
	}
	return queryPlan{
		Types:            types,
		TargetRPS:        rates,
		Duration:         f.duration,
		Warmup:           f.warmup,
		LedgersSpan:      f.ledgersSpan,
		TxPageSpan:       f.txPageSpan,
		TxPageLimit:      f.txPageLimit,
		EventsLimit:      f.eventsLimit,
		NotFoundFraction: f.notFoundFraction,
		TxHashPoolSize:   f.txHashPoolSize,
		Passphrase:       f.passphrase,
		Seed:             f.seed,
	}, nil
}

func validateTxHashPoolSize(size int) error {
	if size < 1 || size > maxTxHashPoolSize {
		return fmt.Errorf("--txhash-pool-size must be in [1, %d], got %d", maxTxHashPoolSize, size)
	}
	return nil
}

// parseQueryTypes splits --types, in the caller's order. A repeat is an error:
// it would duplicate a scenario's rows.
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
			return nil, fmt.Errorf("--types repeats %q, which would duplicate its scenario rows", qtype)
		}
		types = append(types, qtype)
	}
	return types, nil
}

// parseTargetRPS splits --target-rps into rates, in the caller's order. A repeat
// is an error: it would duplicate a scenario's rows.
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
		if err := checkTargetRPS(rps); err != nil {
			return nil, err
		}
		if slices.Contains(rates, rps) {
			return nil, fmt.Errorf("--target-rps repeats %v, which would duplicate its scenario rows", rps)
		}
		rates = append(rates, rps)
	}
	return rates, nil
}

// checkTargetRPS checks one --target-rps rate: positive, finite and at most
// maxTargetRPS.
func checkTargetRPS(rps float64) error {
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		return fmt.Errorf("--target-rps rates must be positive and finite, got %v", rps)
	}
	if rps > maxTargetRPS {
		return fmt.Errorf("--target-rps rates must be <= %v, got %v", float64(maxTargetRPS), rps)
	}
	return nil
}
