package bench

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"
)

// minLegSamples is a basic warning threshold, not a statistical quality gate.
const minLegSamples = 100

// runQueryLegs runs every type at every rate. A type's corpus is built once and
// shared by every rate.
func runQueryLegs(
	ctx context.Context, logger *supportlog.Entry, f *queryFixture, p queryPlan, sink *csvSink,
) error {
	for _, qtype := range p.Types {
		req, err := newQueryRequest(ctx, logger, f, p, qtype)
		if err != nil {
			return fmt.Errorf("prepare the %s benchmark: %w", qtype, err)
		}
		for _, rps := range p.TargetRPS {
			if err := runQueryLeg(ctx, logger, f, p, sink, qtype, rps, req); err != nil {
				return fmt.Errorf("query %s at %s rps: %w", qtype, formatRPS(rps), err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// runQueryLeg runs one (type, rate) leg: page-cache eviction for a cold run,
// then p.Warmup unmeasured requests and the measured ones, dispatched at rps
// over p.Duration. The seed mixes in the type.
func runQueryLeg(
	ctx context.Context, logger *supportlog.Entry, f *queryFixture, p queryPlan, sink *csvSink,
	qtype string, rps float64, req queryRequest,
) error {
	if p.Evict {
		start := time.Now()
		evicted, err := f.evictColdArtifacts()
		if err != nil {
			return err
		}
		sink.observe(fileDriver, driverQueryEvict, time.Since(start), evicted)
	}
	measured := measuredRequests(rps, p.Duration)
	logger.Infof("query %s at %s rps for %s: %d measured requests, %d warmup",
		qtype, formatRPS(rps), p.Duration, measured, p.Warmup)
	res, err := runPacedLeg(ctx, rps, p.Duration, p.Warmup, legSeed(p.Seed, qtype), req)
	if err != nil {
		return err
	}
	recordLeg(sink, qtype, rps, res)
	logQueryLeg(logger, qtype, rps, p.MissFraction, res)
	if res.errs > 0 {
		return fmt.Errorf("%d of %d requests failed: %w", res.errs, res.dispatched, res.firstErr)
	}
	return nil
}

// logQueryLeg keeps a leg's outcomes next to the successful latency rows,
// including legs whose request failures cause a partial report. Warmup
// outcomes are logged apart: they are in no row.
func logQueryLeg(logger *supportlog.Entry, qtype string, rps, missFraction float64, res legResult) {
	logger.Infof("query %s at %s rps outcome: scheduled=%d dispatched=%d successful=%d failed=%d shed=%d "+
		"arrival=%s elapsed=%s drain=%s completion_rps=%.3f",
		qtype, formatRPS(rps), res.scheduled, res.dispatched, len(res.samples), res.errs, res.shed,
		res.offered, res.elapsed, res.drain, float64(achievedMilliRPS(len(res.samples), res.elapsed))/milliPerUnit)
	if res.errs > 0 {
		logger.Warnf("query %s at %s rps: %d of %d requests failed, first error: %v",
			qtype, formatRPS(rps), res.errs, res.dispatched, res.firstErr)
	}
	if res.shed > 0 {
		logger.Warnf("query %s at %s rps shed %d requests: latency rows exclude shed and failed requests",
			qtype, formatRPS(rps), res.shed)
	}
	if res.warmupShed > 0 || res.warmupErrs > 0 {
		logger.Warnf("query %s at %s rps warmup: %d shed, %d failed (warmup is unmeasured); "+
			"first warmup error: %v", qtype, formatRPS(rps), res.warmupShed, res.warmupErrs, res.firstWarmupErr)
	}
	stages := []string{""}
	if qtype == queryTypeTxHash {
		stages = append(stages, txHashStageFound, txHashStageMiss)
	}
	for _, stage := range stages {
		label := queryTotalRow(rps)
		if stage != "" {
			label = queryStageRow(stage, rps)
		}
		var latency series
		for _, s := range res.samples {
			if stage == "" || s.stage == stage {
				latency.observe(s.scheduled, s.items)
			}
		}
		if r, ok := aggregate(label, &latency, false); ok {
			logRow(logger, qtype, r)
		}
		if n := len(latency.samples); n < minLegSamples && stageWarrantsWarning(stage, res.scheduled, missFraction) {
			logger.Warnf("query %s %s has %d successful samples, fewer than %d; "+
				"percentiles have limited sample coverage (not a statistical quality gate)",
				qtype, label, n, minLegSamples)
		}
	}
}

// stageWarrantsWarning reports whether a thin sample count is worth a warning.
// The total row always is. A txhash sub-stage is only when the plan predicted
// at least minLegSamples for it: the miss fraction's share of the schedule, or
// its complement for the found stage. Below that the shortfall is the rate,
// duration and miss fraction the operator asked for.
func stageWarrantsWarning(stage string, scheduled int, missFraction float64) bool {
	var share float64
	switch stage {
	case txHashStageMiss:
		share = missFraction
	case txHashStageFound:
		share = 1 - missFraction
	default:
		return true
	}
	return float64(scheduled)*share >= minLegSamples
}

// legSeed is a leg's seed: the run seed plus the type's index in allQueryTypes.
// Distinct types get distinct seeds.
func legSeed(base int64, qtype string) int64 {
	return base + int64(slices.Index(allQueryTypes, qtype))
}

// recordLeg files one leg's samples into the report.
//
// The type's CSV: every successful request lands in total_r<rate> (scheduled latency, the
// row the results converter reads) and service_r<rate> (service time); a
// request carrying a stage lands in <stage>_r<rate> too.
//
// driver.csv records wall, the _millirps window ratio, lag and shed count.
// Wall and _millirps keep their successful-request n_items. Lag includes shed
// positions. query-accounting.csv records measured counts (including shed),
// arrival, elapsed and drain windows, and target and completion rates.
func recordLeg(sink *csvSink, qtype string, rps float64, res legResult) {
	total := queryTotalRow(rps)
	service := queryServiceRow(rps)
	for _, s := range res.samples {
		sink.observe(qtype, total, s.scheduled, s.items)
		sink.observe(qtype, service, s.service, s.items)
		if s.stage != "" {
			sink.observe(qtype, queryStageRow(s.stage, rps), s.scheduled, s.items)
		}
	}

	answered := len(res.samples)
	sink.observe(fileDriver, queryDriverRow(qtype, rps), res.wall, answered)
	sink.observe(fileDriver, queryDriverLegRow(qtype, rps, driverLegRPSSuffix),
		achievedMilliRPS(answered, res.offered), answered)
	sink.observe(fileDriver, queryDriverLegRow(qtype, rps, driverLegShedSuffix), 0, res.shed)
	sink.observe(fileQueryAccounting, queryDriverLegRow(qtype, rps, driverLegTargetRPSSuffix),
		time.Duration(math.Round(rps*milliPerUnit)), 0)
	sink.observe(fileQueryAccounting, queryDriverLegRow(qtype, rps, driverLegCompletionRPSSuffix),
		achievedMilliRPS(answered, res.elapsed), 0)
	for _, counter := range []struct {
		suffix string
		count  int
	}{
		{driverLegScheduledSuffix, res.scheduled},
		{driverLegDispatchedSuffix, res.dispatched},
		{driverLegSuccessSuffix, answered},
		{driverLegFailedSuffix, res.errs},
		{driverLegShedSuffix, res.shed},
	} {
		sink.observe(fileQueryAccounting, queryDriverLegRow(qtype, rps, counter.suffix), 0, counter.count)
	}
	for _, window := range []struct {
		suffix string
		d      time.Duration
	}{
		{driverLegOfferedSuffix, res.offered},
		{driverLegElapsedSuffix, res.elapsed},
		{driverLegDrainSuffix, res.drain},
	} {
		sink.observe(fileQueryAccounting, queryDriverLegRow(qtype, rps, window.suffix), window.d, 0)
	}
	for _, lag := range res.lags {
		sink.observe(fileDriver, queryDriverLegRow(qtype, rps, driverLegLagSuffix), lag, 1)
	}
}

// achievedMilliRPS is answered/offered in requests per second, scaled by
// milliPerUnit and carried as a Duration. No offered window reports zero.
func achievedMilliRPS(answered int, offered time.Duration) time.Duration {
	if offered <= 0 {
		return 0
	}
	return time.Duration(math.Round(float64(answered) / offered.Seconds() * milliPerUnit))
}

// warnFixedReadRange reports the configured types whose span covers the whole
// fixture. pickStart returns the range's first ledger for those, so every
// request reads the same ledgers and their percentiles measure a cached read.
func warnFixedReadRange(logger *supportlog.Entry, f *queryFixture, p queryPlan) {
	room := f.LastLedger - f.FirstLedger + 1
	var fixed []string
	for _, qtype := range p.Types {
		var span uint32
		switch qtype {
		case queryTypeLedgers:
			span = p.LedgersSpan
		case queryTypeTxPage:
			span = p.TxPageSpan
		default:
			continue
		}
		if span >= room {
			fixed = append(fixed, qtype)
		}
	}
	if len(fixed) == 0 {
		return
	}
	logger.Warnf("fixture holds %d ledgers; %s read the same fixed range every request (span >= range), "+
		"so their percentiles measure a cached read", room, strings.Join(fixed, " and "))
	if p.Extra != nil {
		p.Extra["fixedReadRange"] = strings.Join(fixed, ",")
	}
}

// runQueryBench is the body both subcommands share: prepare --out, open the
// fixture (timed into driver.csv), run the legs, report. A failure after the
// fixture opens still writes the partial report and the peak RSS.
func runQueryBench(
	ctx context.Context, logger *supportlog.Entry, p queryPlan, outDir string,
	open func() (*queryFixture, func(), error),
) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create --out dir %s: %w", outDir, err)
	}
	sink := newSchemaCSVSink(querySpecs(p.Types, p.TargetRPS))

	start := time.Now()
	f, release, err := open()
	if err != nil {
		return err
	}
	defer release()
	sink.observe(fileDriver, driverQueryOpen, time.Since(start), len(f.Chunks))
	logger.Infof("serving ledgers [%d, %d] over %d chunk(s)", f.FirstLedger, f.LastLedger, len(f.Chunks))
	warnFixedReadRange(logger, f, p)

	err = runQueryLegs(ctx, logger, f, p, sink)
	// VmHWM never decreases; a failed run still gets the row.
	recordPeakRSS(logger, sink, readPeakRSS)
	if err != nil {
		writePartialCSVs(logger, sink, outDir)
		return err
	}

	sink.logSummary(logger)
	written, err := sink.writeCSVs(outDir)
	if err != nil {
		return err
	}
	logger.Infof("wrote %d CSVs to %s", len(written), outDir)
	return nil
}
