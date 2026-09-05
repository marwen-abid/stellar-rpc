package bench

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"
)

// minLegSamples is the measured-request count below which a leg's percentiles
// are reported with a warning.
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
	if measured < minLegSamples {
		logger.Warnf("query %s at %s rps measures %d requests: its percentiles rest on fewer than "+
			"%d samples, so a longer --duration makes them steadier",
			qtype, formatRPS(rps), measured, minLegSamples)
	}

	res, err := runPacedLeg(ctx, rps, p.Duration, p.Warmup, p.Seed+int64(len(qtype)), req)
	if err != nil {
		return err
	}
	if res.errs > 0 {
		return fmt.Errorf("%d of %d requests failed", res.errs, res.dispatched)
	}
	recordLeg(sink, qtype, rps, res)
	return nil
}

// recordLeg files one leg's samples into the report.
//
// The type's CSV: every request lands in total_r<rate> (scheduled latency, the
// row the results converter reads) and service_r<rate> (service time); a
// request carrying a stage lands in <stage>_r<rate> too.
//
// driver.csv: <qtype>_r<rate> is the leg wall, to the last completion;
// _millirps is answered requests over the offered window, times 1000; _lag has
// one sample per measured position, shed included; _shed is written for every
// leg.
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
	for _, lag := range res.lags {
		sink.observe(fileDriver, queryDriverLegRow(qtype, rps, driverLegLagSuffix), lag, 1)
	}
	sink.observe(fileDriver, queryDriverLegRow(qtype, rps, driverLegShedSuffix), 0, res.shed)
}

// achievedMilliRPS is answered/offered in requests per second, scaled by
// milliPerUnit and carried as a Duration. No offered window reports zero.
func achievedMilliRPS(answered int, offered time.Duration) time.Duration {
	if offered <= 0 {
		return 0
	}
	return time.Duration(math.Round(float64(answered) / offered.Seconds() * milliPerUnit))
}

// runQueryBench is the body both subcommands share: prepare --out, open the
// fixture (timed into driver.csv), run the legs, report. A failed run still
// writes the partial report and the peak RSS.
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
