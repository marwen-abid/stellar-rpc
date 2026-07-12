package bench

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"
)

// NewCompareCommand returns the `bench-compare` command: it reads the CSV
// reports of repeated bench-ingest/bench-query runs of a baseline build and a
// candidate build, compares each gated metric's per-side medians against a
// noise threshold derived from the runs themselves, and exits non-zero naming
// every regressed metric. See compare.go for the statistics.
func NewCompareCommand() *cobra.Command {
	var (
		baseRoots   []string
		candRoots   []string
		metricsFile string
		minEffect   float64
		maxNoise    float64
		summaryPath string
	)
	cmd := &cobra.Command{
		Use:   "bench-compare",
		Short: "Compare repeated benchmark CSV runs of two builds and flag regressions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			_, stop, logger := benchContext()
			defer stop()
			specs, err := parseMetricsFile(metricsFile)
			if err != nil {
				return err
			}
			sigLevel := alpha / float64(len(specs))
			if n := binomial(len(baseRoots)+len(candRoots), len(candRoots)); n > 0 && 1/float64(n) > sigLevel {
				logger.Warnf("only %d run-label splits — the significance gate can never fire "+
					"(smallest reachable p %.4f > corrected level %.4f for %d metrics); add runs per side",
					n, 1/float64(n), sigLevel, len(specs))
			}
			comps, err := compareMetrics(specs, baseRoots, candRoots, minEffect, maxNoise)
			if err != nil {
				return err
			}
			logComparisons(logger, comps)
			if summaryPath != "" {
				if err := appendSummary(summaryPath, comps, len(baseRoots), len(candRoots)); err != nil {
					return err
				}
			}
			if regressed := regressions(comps); len(regressed) > 0 {
				return fmt.Errorf("performance regression in %d metric(s): %s",
					len(regressed), strings.Join(regressed, ", "))
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringArrayVar(&baseRoots, "baseline-root", nil,
		"one baseline run's CSV root (repeat per run; required)")
	fs.StringArrayVar(&candRoots, "candidate-root", nil,
		"one candidate run's CSV root (repeat per run; required)")
	fs.StringVar(&metricsFile, "metrics-file", "",
		"gated-metric list, one <csv path>,<row>,<column> per line (required)")
	fs.Float64Var(&minEffect, "min-effect", 0.05,
		"relative slowdown below which a delta never fails")
	fs.Float64Var(&maxNoise, "max-noise", 0.10,
		"noise threshold above which a metric is reported unstable instead of gated")
	fs.StringVar(&summaryPath, "summary", "",
		"append a Markdown comparison table to PATH (e.g. $GITHUB_STEP_SUMMARY)")
	markRequired(cmd, "baseline-root", "candidate-root", "metrics-file")
	return cmd
}

func logComparisons(logger *supportlog.Entry, comps []comparison) {
	for _, c := range comps {
		line := fmt.Sprintf("%-45s baseline=%-12s candidate=%-12s delta=%+.1f%% p=%.3f noise=%.1f%% %s",
			c.Spec, formatValue(c.Spec, c.BaseMean), formatValue(c.Spec, c.CandMean),
			100*c.Delta, c.PValue, 100*c.Noise, c.Verdict)
		switch c.Verdict {
		case verdictRegression:
			logger.Error(line)
		case verdictUnstable:
			logger.Warn(line)
		default:
			logger.Info(line)
		}
	}
}

// appendSummary appends the Markdown table to path — appending, not
// truncating, because GitHub's $GITHUB_STEP_SUMMARY is a shared
// append-to file.
func appendSummary(path string, comps []comparison, baseRuns, candRuns int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open summary %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := writeSummaryMarkdown(f, comps, baseRuns, candRuns); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return f.Close()
}
