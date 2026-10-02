package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/ingest"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/observability"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/hotchunk"
)

const csvHeader = "stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns"

// CSV file basenames not named after a data or query type.
const (
	fileHot    = "hot"
	fileDriver = "driver"
)

// driver.csv row labels.
const (
	driverBackfillWall = "backfill_wall" // cold: RunBackfill's plan-and-execute wall (Metrics.Freeze)
	driverIndexRebuild = "index_rebuild" // cold: one txhash index build, including its eager sweep
	driverChunkTotal   = "chunk_total"   // cold: per-chunk ColdService lifetime
	driverTotalSuffix  = "_total"        // cold: "<type>_total", from ColdIngest
	driverColdExtract  = "cold_extract"  // cold: the per-ledger ExtractLedgerTxParts walk, shared by all types
	driverIngestTotal  = "ingest_total"  // hot: per-ledger sum of one HotPhase burst
	driverRunWall      = "run_wall"      // hot: whole-run wall-clock
	driverPaceLag      = "pace_lag"      // hot, paced runs: per-ledger lag behind the close schedule at commit
	// The run's peak resident set size (VmHWM). Its duration columns carry
	// bytes, not nanoseconds.
	driverPeakRSS = "peak_rss_bytes"
)

// Cold data-type and stage labels. They must equal the strings the ingest
// engine emits through MetricSink, because they set the report order. A label
// that drifts is still reported, after the known ones (see withUnknown).
const (
	coldTypeLedgers = "ledgers"
	coldTypeTxhash  = "txhash"
	coldTypeEvents  = "events"

	coldStageTermIndex = "term_index"
	coldStageWrite     = "write"
	coldStageFinalize  = "finalize"
)

// fileSpec is one CSV file of the report: its basename (without .csv) and the
// fixed top-to-bottom order of its rows.
type fileSpec struct {
	name     string
	rowOrder []string
}

// fileSpecs is the bench-ingest report schema: one CSV per cold data type with
// one row per cold stage, hot.csv with one row per hotchunk.Phase, and
// driver.csv. driver.csv lists the cold rows, then the hot rows, then
// peak_rss_bytes, which both modes emit. A row with no samples is suppressed,
// so each mode's report has only its own rows.
//
//nolint:gochecknoglobals // fixed report schema, read-only
var fileSpecs = func() []fileSpec {
	coldTypes := []string{coldTypeLedgers, coldTypeTxhash, coldTypeEvents}
	coldStages := []string{coldStageTermIndex, coldStageWrite, coldStageFinalize}

	hotRows := make([]string, hotchunk.NumPhases)
	for p := range hotchunk.NumPhases {
		hotRows[p] = p.String()
	}

	driverRows := make([]string, 0, len(coldTypes)+8)
	driverRows = append(driverRows, driverBackfillWall, driverIndexRebuild, driverChunkTotal)
	for _, dt := range coldTypes {
		driverRows = append(driverRows, dt+driverTotalSuffix)
	}
	driverRows = append(driverRows, driverColdExtract,
		driverIngestTotal, driverRunWall, driverPaceLag, driverPeakRSS)

	specs := make([]fileSpec, 0, len(coldTypes)+2)
	for _, dt := range coldTypes {
		specs = append(specs, fileSpec{name: dt, rowOrder: coldStages})
	}
	return append(specs,
		fileSpec{name: fileHot, rowOrder: hotRows},
		fileSpec{name: fileDriver, rowOrder: driverRows},
	)
}()

// querySpecs is the bench-query report schema for one run over types and
// rates. Types must be distinct and so must rates, or the schema repeats a row
// name. The external results converter reads the query-type CSVs and
// driver.csv. It recognizes only wall, _millirps, _lag and _shed as driver leg
// rows and classifies every other driver row as setup. So the driver.csv row
// set stays fixed, and completion accounting goes in query-accounting.csv.
func querySpecs(types []string, rates []float64) []fileSpec {
	specs := make([]fileSpec, 0, len(types)+2)
	legSuffixes := []string{driverLegRPSSuffix, driverLegLagSuffix, driverLegShedSuffix}
	accountingSuffixes := slices.Concat(
		[]string{driverLegTargetRPSSuffix, driverLegCompletionRPSSuffix},
		driverLegCountSuffixes,
		[]string{driverLegArrivalSuffix, driverLegElapsedSuffix, driverLegDrainSuffix},
	)
	driverRows := make([]string, 0, len(types)*len(rates)*(len(legSuffixes)+1)+3)
	accountingRows := make([]string, 0, len(types)*len(rates)*len(accountingSuffixes))
	driverRows = append(driverRows, driverQueryOpen, driverQueryEvict)
	for _, qtype := range types {
		rows := make([]string, 0, len(rates)*2)
		for _, rps := range rates {
			rows = append(rows, queryTotalRow(rps))
		}
		for _, rps := range rates {
			rows = append(rows, queryServiceRow(rps))
		}
		if qtype == queryTypeTxHash {
			for _, stage := range []string{txHashStageFound, txHashStageMiss} {
				for _, rps := range rates {
					rows = append(rows, queryStageRow(stage, rps))
				}
			}
		}
		for _, rps := range rates {
			driverRows = append(driverRows, queryDriverRow(qtype, rps))
			for _, suffix := range legSuffixes {
				driverRows = append(driverRows, queryDriverLegRow(qtype, rps, suffix))
			}
			for _, suffix := range accountingSuffixes {
				accountingRows = append(accountingRows, queryDriverLegRow(qtype, rps, suffix))
			}
		}
		specs = append(specs, fileSpec{name: qtype, rowOrder: rows})
	}
	driverRows = append(driverRows, driverPeakRSS)
	return append(specs,
		fileSpec{name: fileDriver, rowOrder: driverRows},
		fileSpec{name: fileQueryAccounting, rowOrder: accountingRows},
	)
}

// sample is one observed (duration, item-count) pair.
type sample struct {
	d     time.Duration
	items int
}

// series accumulates samples for one CSV row.
type series struct {
	samples []sample
}

func (s *series) observe(d time.Duration, items int) {
	s.samples = append(s.samples, sample{d: d, items: items})
}

// rowKey locates one CSV row: the file basename it lands in and its row label.
type rowKey struct {
	file, row string
}

// csvSink is an ingest.MetricSink and an observability.Metrics. It keeps every
// signal in memory as a raw sample and, on writeCSVs, aggregates each row into
// percentiles laid out per specs. n counts the included samples and n_items
// sums their item counts. Zero-duration samples are dropped, so that empty
// ledger stages do not skew percentiles, except in rows where keepsZeroSamples
// holds. Rows with no included samples, and files with no rows, are suppressed.
//
// Of the observability signals, csvSink records Freeze, Rebuild and
// LastCommitted and drops the rest. Prune fires from each index build's eager
// sweep, but Rebuild already includes that wall.
//
// All methods are safe for concurrent use: the backfill scheduler runs several
// chunk freezes against one sink.
type csvSink struct {
	specs []fileSpec // read-only after construction

	mu       sync.Mutex
	rows     map[rowKey]*series
	hotBurst time.Duration // the current hot ledger's phase sum (see HotPhase)

	// lastSeq is the last Metrics.LastCommitted value. The hot driver uses it
	// to check that the run reached its final ledger.
	lastSeq atomic.Uint32

	// schedule is the paced hot run's close schedule; nil when the run is not
	// paced.
	schedule *paceSchedule
}

var (
	_ ingest.MetricSink     = (*csvSink)(nil)
	_ observability.Metrics = (*csvSink)(nil)
)

// newCSVSink returns an empty recorder with the bench-ingest schema.
func newCSVSink() *csvSink {
	return newSchemaCSVSink(fileSpecs)
}

// newSchemaCSVSink returns an empty recorder with schema specs.
func newSchemaCSVSink(specs []fileSpec) *csvSink {
	return &csvSink{rows: make(map[rowKey]*series), specs: specs}
}

// HotPhase records one hot ingest phase into hot.csv. It also sums each
// ledger's phases into one driver.csv ingest_total sample, because per-phase
// percentiles do not add up to a per-ledger total.
//
// The production hot loop is one goroutine, so phases arrive as one burst per
// ledger in hotchunk.Phase order. PhaseExtract starts a burst. PhaseApply ends
// it and is emitted only on success, so a failed ledger records no
// ingest_total. Interleaved bursts from several goroutines are safe but sum
// across ledgers.
func (s *csvSink) HotPhase(phase hotchunk.Phase, d time.Duration, items int, _ error) {
	s.observe(fileHot, phase.String(), d, items)

	// Release mu before observe takes it again.
	s.mu.Lock()
	if phase == hotchunk.PhaseExtract {
		s.hotBurst = 0
	}
	s.hotBurst += d
	total := s.hotBurst
	s.mu.Unlock()

	if phase == hotchunk.PhaseApply {
		s.observe(fileDriver, driverIngestTotal, total, 1)
	}
}

// ColdIngest records one cold ingester's per-chunk total.
func (s *csvSink) ColdIngest(dataType string, d time.Duration, items int, _ error) {
	s.observe(fileDriver, dataType+driverTotalSuffix, d, items)
}

// ColdChunkTotal records the per-chunk aggregate wall-clock.
func (s *csvSink) ColdChunkTotal(d time.Duration) {
	s.observe(fileDriver, driverChunkTotal, d, 0)
}

// ColdExtract records the per-ledger ExtractLedgerTxParts walk that all data
// types share.
func (s *csvSink) ColdExtract(d time.Duration, items int, _ error) {
	s.observe(fileDriver, driverColdExtract, d, items)
}

// IngestStage records one cold ingester's per-stage wall-clock.
func (s *csvSink) IngestStage(dataType, stage string, d time.Duration, items int) {
	s.observe(dataType, stage, d, items)
}

// Freeze records one backfill pass's whole plan-and-execute wall-clock.
func (s *csvSink) Freeze(d time.Duration) {
	s.observe(fileDriver, driverBackfillWall, d, 0)
}

// Rebuild records one txhash index build's wall-clock (including its eager
// post-build sweep).
func (s *csvSink) Rebuild(d time.Duration) {
	s.observe(fileDriver, driverIndexRebuild, d, 0)
}

// LastCommitted stores the hot loop's committed gauge and, in a paced run,
// records that ledger's pace lag.
func (s *csvSink) LastCommitted(lastCommitted uint32) {
	s.lastSeq.Store(lastCommitted)
	if s.schedule != nil {
		s.recordPaceLag(lastCommitted)
	}
}

func (s *csvSink) RetentionFloor(uint32) {}

func (s *csvSink) ChunkBoundary() {}

func (s *csvSink) LiveHotChunks(int) {}

func (s *csvSink) BackfillPass(time.Duration) {}

func (s *csvSink) Discard(int, time.Duration) {}

func (s *csvSink) Prune(int, time.Duration) {}

func (s *csvSink) FailedDestroy() {}

func (s *csvSink) TxIndexInconsistency() {}

// observe appends one sample to the (file, row) series.
func (s *csvSink) observe(fileName, rowName string, d time.Duration, items int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := rowKey{file: fileName, row: rowName}
	sr := s.rows[k]
	if sr == nil {
		sr = &series{}
		s.rows[k] = sr
	}
	sr.observe(d, items)
}

// lastCommittedSeq returns the highest ledger the hot loop reported committed.
func (s *csvSink) lastCommittedSeq() uint32 {
	return s.lastSeq.Load()
}

// sumDriver returns the summed duration of a driver row's samples — the
// numerator of the cold driver's effective-concurrency summary.
func (s *csvSink) sumDriver(name string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total time.Duration
	if sr := s.rows[rowKey{file: fileDriver, row: name}]; sr != nil {
		for _, sm := range sr.samples {
			total += sm.d
		}
	}
	return total
}

// recordPaceLag records how far past its due time ledger seq committed,
// clamped at zero, as one pace_lag sample. A lag that grows across the run
// means ingestion does not keep up with the close cadence.
func (s *csvSink) recordPaceLag(seq uint32) {
	due, ok := s.schedule.dueForSeq(seq)
	if !ok {
		return
	}
	lag := max(s.schedule.clock().Sub(due), 0)
	s.observe(fileDriver, driverPaceLag, lag, 1)
}

// row is one aggregated CSV row.
type row struct {
	name  string
	n     int
	items int
	total time.Duration
	p50   time.Duration
	p90   time.Duration
	p99   time.Duration
	maxv  time.Duration
}

// aggregate reduces a series to a row. Zero-duration samples are dropped unless
// includeZeros (see keepsZeroSamples). ok is false when no sample survives.
func aggregate(name string, s *series, includeZeros bool) (row, bool) {
	durs := make([]time.Duration, 0, len(s.samples))
	items := 0
	for _, sm := range s.samples {
		if sm.d > 0 || includeZeros {
			durs = append(durs, sm.d)
			items += sm.items
		}
	}
	if len(durs) == 0 {
		return row{}, false
	}
	slices.Sort(durs)
	var total time.Duration
	for _, d := range durs {
		total += d
	}
	pick := func(p float64) time.Duration {
		i := int(p * float64(len(durs)))
		if i >= len(durs) {
			i = len(durs) - 1
		}
		return durs[i]
	}
	return row{
		name: name, n: len(durs), items: items, total: total,
		p50: pick(0.50), p90: pick(0.90), p99: pick(0.99), maxv: durs[len(durs)-1],
	}, true
}

// withUnknown returns order followed by the sorted keys of m that order does
// not contain.
func withUnknown[V any](order []string, m map[string]V) []string {
	var extra []string
	for k := range m {
		if !slices.Contains(order, k) {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return order
	}
	slices.Sort(extra)
	return append(slices.Clone(order), extra...)
}

// keepsZeroSamples reports whether a row keeps its zero-duration samples,
// because zero is a real observation there: the lag (including pace_lag),
// drain, rate and count rows of driver.csv and query-accounting.csv.
func keepsZeroSamples(file, label string) bool {
	if file != fileDriver && file != fileQueryAccounting {
		return false
	}
	_, isCount := legCountName(label)
	return isCount ||
		strings.HasSuffix(label, driverLegLagSuffix) ||
		strings.HasSuffix(label, driverLegDrainSuffix) ||
		strings.HasSuffix(label, driverLegRPSSuffix)
}

// driverLegCountSuffixes are the leg rows that store a request count in
// n_items, with duration zero.
//
//nolint:gochecknoglobals // fixed report vocabulary, read-only
var driverLegCountSuffixes = []string{
	driverLegScheduledSuffix, driverLegDispatchedSuffix, driverLegSuccessSuffix,
	driverLegFailedSuffix, driverLegShedSuffix,
}

// legCountName returns the count that a count row stores, such as "shed" for a
// _shed row. ok is false for any other row.
func legCountName(label string) (string, bool) {
	for _, suffix := range driverLegCountSuffixes {
		if strings.HasSuffix(label, suffix) {
			return strings.TrimPrefix(suffix, "_"), true
		}
	}
	return "", false
}

// file is one aggregated CSV file: its basename (without .csv) and its rows.
type file struct {
	name string
	rows []row
}

// files aggregates every recorded series into the report's CSV files, in the
// sink's schema order (a file outside the schema is appended after, sorted, its
// rows ordered by sorted label).
func (s *csvSink) files() []file {
	s.mu.Lock()
	defer s.mu.Unlock()

	byFile := make(map[string]map[string]*series)
	for k, sr := range s.rows {
		if byFile[k.file] == nil {
			byFile[k.file] = make(map[string]*series)
		}
		byFile[k.file][k.row] = sr
	}

	names := make([]string, len(s.specs))
	rowOrders := make(map[string][]string, len(s.specs))
	for i, spec := range s.specs {
		names[i] = spec.name
		rowOrders[spec.name] = spec.rowOrder
	}

	var out []file
	for _, name := range withUnknown(names, byFile) {
		byRow := byFile[name]
		var rows []row
		for _, label := range withUnknown(rowOrders[name], byRow) {
			if sr := byRow[label]; sr != nil {
				if r, ok := aggregate(label, sr, keepsZeroSamples(name, label)); ok {
					rows = append(rows, r)
				}
			}
		}
		if len(rows) > 0 {
			out = append(out, file{name: name, rows: rows})
		}
	}
	return out
}

// writeCSVs writes every non-empty aggregated CSV under outDir (created if
// missing) and returns the files it wrote.
func (s *csvSink) writeCSVs(outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	var written []string
	for _, f := range s.files() {
		path := filepath.Join(outDir, f.name+".csv")
		if err := writeCSV(path, f.rows); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

func writeCSV(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintln(f, csvHeader); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(f, "%s,%d,%d,%d,%d,%d,%d,%d\n",
			r.name, r.n, r.items, r.total.Nanoseconds(),
			r.p50.Nanoseconds(), r.p90.Nanoseconds(), r.p99.Nanoseconds(), r.maxv.Nanoseconds(),
		); err != nil {
			return fmt.Errorf("write row %s: %w", r.name, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// logSummary logs one line per aggregated row.
func (s *csvSink) logSummary(logger *supportlog.Entry) {
	for _, f := range s.files() {
		for _, r := range f.rows {
			logRow(logger, f.name, r)
		}
	}
}

// logRow logs one aggregated row. The rate and count rows of driver.csv and
// query-accounting.csv, and driver.csv's peak_rss_bytes, print in their own
// units.
func logRow(logger *supportlog.Entry, fileName string, r row) {
	if fileName == fileDriver || fileName == fileQueryAccounting {
		count, isCount := legCountName(r.name)
		switch {
		case fileName == fileDriver && r.name == driverPeakRSS:
			logger.Infof("%-10s %-12s n=%-7d bytes=%d", fileName, r.name, r.n, r.total.Nanoseconds())
			return
		case strings.HasSuffix(r.name, driverLegTargetRPSSuffix),
			strings.HasSuffix(r.name, driverLegCompletionRPSSuffix):
			logger.Infof("%-10s %-12s n=%-7d rps=%.3f", fileName, r.name, r.n,
				float64(r.total.Nanoseconds())/milliPerUnit)
			return
		case strings.HasSuffix(r.name, driverLegRPSSuffix):
			logger.Infof("%-10s %-12s n=%-7d window_rps=%.3f", fileName, r.name, r.n,
				float64(r.total.Nanoseconds())/milliPerUnit)
			return
		case isCount:
			logger.Infof("%-10s %-12s n=%-7d %s=%d", fileName, r.name, r.n, count, r.items)
			return
		}
	}
	logger.Infof("%-10s %-12s n=%-7d items=%-9d total=%-12s p50=%-10s p90=%-10s p99=%-10s max=%s",
		fileName, r.name, r.n, r.items,
		r.total.Round(time.Microsecond),
		r.p50.Round(time.Microsecond),
		r.p90.Round(time.Microsecond),
		r.p99.Round(time.Microsecond),
		r.maxv.Round(time.Microsecond))
}
