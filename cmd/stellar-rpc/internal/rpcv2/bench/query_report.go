package bench

import "strconv"

// fileQueryAccounting holds per-leg rates, request counts and timing windows.
const fileQueryAccounting = "query-accounting"

// Query types, in report order. Each is a --types value and a report CSV
// basename (see querySpecs).
const (
	// queryTypeLedgers: ReadView.ScanLedgers, getLedgers' path.
	queryTypeLedgers = "ledgers"
	// queryTypeTxPage: getTransactions' paged ledger walk.
	queryTypeTxPage = "txpage" //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
	// queryTypeTxHash: getTransaction's by-hash lookup, the MPHF candidate
	// verified against the ledger.
	queryTypeTxHash = "txhash"
	// queryTypeEvents: ReadView.QueryEvents.
	queryTypeEvents = "events" //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
)

// allQueryTypes is every --types value, in report order.
//
//nolint:gochecknoglobals,unused // fixed vocabulary, read-only; consumed by bench-query/02-read-path
var allQueryTypes = []string{queryTypeLedgers, queryTypeTxPage, queryTypeTxHash, queryTypeEvents}

// Query report row labels, the results converter's contract (see querySpecs).
// <rate> is the leg's target rate as formatRPS spells it.
const (
	queryRowTotalPrefix          = "total_r"
	queryRowServicePrefix        = "service_r"
	driverLegRPSSuffix           = "_millirps"
	driverLegTargetRPSSuffix     = "_target_millirps"
	driverLegCompletionRPSSuffix = "_completion_millirps"
	driverLegScheduledSuffix     = "_scheduled"
	driverLegDispatchedSuffix    = "_dispatched"
	driverLegSuccessSuffix       = "_successful"
	driverLegFailedSuffix        = "_failed"
	driverLegOfferedSuffix       = "_arrival"
	driverLegElapsedSuffix       = "_elapsed"
	driverLegDrainSuffix         = "_drain"
	driverLegLagSuffix           = "_lag"
	driverLegShedSuffix          = "_shed"
	driverQueryOpen              = "open"  // fixture open: catalog, handles, first read view
	driverQueryEvict             = "evict" // one page-cache eviction pass before a cold leg
)

// txhash sub-stage labels; total_r<rate> blends both.
const (
	txHashStageFound = "found"
	txHashStageMiss  = "miss"
)

// formatRPS renders a rate as its row label spells it: the shortest decimal
// that round-trips.
func formatRPS(rps float64) string { return strconv.FormatFloat(rps, 'f', -1, 64) }

// queryTotalRow is a per-type CSV's scheduled-latency row for the leg at rps.
func queryTotalRow(rps float64) string { return queryRowTotalPrefix + formatRPS(rps) }

// queryServiceRow is a per-type CSV's service-time row for the leg at rps.
func queryServiceRow(rps float64) string { return queryRowServicePrefix + formatRPS(rps) }

// queryStageRow is a per-type CSV's row for one sub-stage of the leg at rps.
func queryStageRow(stage string, rps float64) string { return stage + "_r" + formatRPS(rps) }

// queryDriverRow is driver.csv's wall-clock row for one query type's leg at rps.
func queryDriverRow(qtype string, rps float64) string { return qtype + "_r" + formatRPS(rps) }

// queryDriverLegRow names one leg metric in driver.csv or query-accounting.csv.
func queryDriverLegRow(qtype string, rps float64, suffix string) string {
	return queryDriverRow(qtype, rps) + suffix
}

// milliPerUnit is the scale of all rate rows, stored in the duration columns.
const milliPerUnit = 1000
