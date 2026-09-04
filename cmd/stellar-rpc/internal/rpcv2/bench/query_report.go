package bench

import "strconv"

// The query types --types selects, in report order. Each names one read path
// through query.ReadView and is also a report CSV basename (see querySpecs).
const (
	// queryTypeLedgers: point reads and fixed-length range scans over
	// ReadView.ScanLedgers, getLedgers' path.
	queryTypeLedgers = "ledgers"
	// queryTypeTxPage: the paged ledger walk getTransactions performs,
	// extracting each ledger's transactions in sequence.
	queryTypeTxPage = "txpage" //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
	// queryTypeTxHash: the full by-hash lookup getTransaction performs: hot
	// indexes first, then the frozen window indexes with the MPHF candidate
	// verified against the ledger. Never an index probe alone.
	queryTypeTxHash = "txhash"
	// queryTypeEvents: ReadView.QueryEvents over a filter set derived from the
	// benchmarked chunk.
	queryTypeEvents = "events" //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
)

// allQueryTypes is every type --types accepts, in report order. It is also the
// list the campaign runner passes verbatim.
//
//nolint:gochecknoglobals,unused // fixed vocabulary, read-only; consumed by bench-query/02-read-path
var allQueryTypes = []string{queryTypeLedgers, queryTypeTxPage, queryTypeTxHash, queryTypeEvents}

// Query report row labels, the results converter's contract. A row belonging to
// one leg carries an _r<rate> segment holding the leg's target rate as
// --target-rps spelled it: a per-type CSV names its latency rows total_r<rate>
// and service_r<rate>; driver.csv names a leg's wall row <qtype>_r<rate> and
// its metrics with the three suffixes below. A driver row with no _r<rate>
// segment belongs to the run's setup.
const (
	queryRowTotalPrefix   = "total_r"
	queryRowServicePrefix = "service_r"
	driverLegRPSSuffix    = "_millirps"
	driverLegLagSuffix    = "_lag"
	driverLegShedSuffix   = "_shed"
	driverQueryOpen       = "open"  // fixture open: catalog, handles, first read view
	driverQueryEvict      = "evict" // one page-cache eviction pass before a cold leg
)

// Sub-stage labels for txhash. total_r<rate> blends hits and misses; these two
// split it, since a hit and a miss are different amounts of work and a blended
// p99 hides which one moved.
const (
	txHashStageFound = "found"
	txHashStageMiss  = "miss"
)

// formatRPS renders a target rate as its row label spells it: the shortest
// decimal that reads back as the same rate (0.5 stays "0.5", 300 stays "300").
func formatRPS(rps float64) string { return strconv.FormatFloat(rps, 'f', -1, 64) }

// queryTotalRow is a per-type CSV's scheduled-latency row for the leg at rps.
func queryTotalRow(rps float64) string { return queryRowTotalPrefix + formatRPS(rps) }

// queryServiceRow is a per-type CSV's service-time row for the leg at rps.
func queryServiceRow(rps float64) string { return queryRowServicePrefix + formatRPS(rps) }

// queryStageRow is a per-type CSV's row for one sub-stage of the leg at rps.
func queryStageRow(stage string, rps float64) string { return stage + "_r" + formatRPS(rps) }

// queryDriverRow is driver.csv's wall-clock row for one query type's leg at rps.
func queryDriverRow(qtype string, rps float64) string { return qtype + "_r" + formatRPS(rps) }

// queryDriverLegRow is driver.csv's row for one of a leg's driver metrics:
// queryDriverRow's label plus the metric's suffix.
func queryDriverLegRow(qtype string, rps float64, suffix string) string {
	return queryDriverRow(qtype, rps) + suffix
}

// milliPerUnit is the scale the _millirps row stores an achieved rate at, so a
// fractional rate survives an integer CSV column.
const milliPerUnit = 1000
