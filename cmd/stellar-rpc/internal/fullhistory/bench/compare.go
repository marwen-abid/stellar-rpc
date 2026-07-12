package bench

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// compare.go implements the paired-run comparison behind `bench-compare`:
// baseline and candidate builds are benchmarked several times each on the
// same machine (interleaved, against the same dataset), and each gated metric
// is judged on three axes:
//
//   - significance: an exact permutation test on the per-run values — the
//     observed relative difference of the side means must be more extreme
//     than the label-blind reshuffles allow at the Bonferroni-corrected
//     level alpha/m (m = gated metrics; without the correction a ~20-metric
//     gate false-positives on most runs). A delta a single outlier run can
//     explain does not pass this.
//   - size: deltas below --min-effect never fail, however significant —
//     too small to matter.
//   - benchmark health: a metric whose within-side spread exceeds
//     --max-noise is reported unstable and never gated — a too-noisy
//     benchmark is a defect of the benchmark, not of the change under test.

// metricSpec locates one gated value: a CSV file relative to each run root, a
// row label in its first column, and one of the schema's value columns.
type metricSpec struct {
	File   string // e.g. "ingest-cold/ledgers.csv"
	Row    string // e.g. "write"
	Column string // e.g. "total_ns"
}

func (m metricSpec) String() string {
	return m.File + ":" + m.Row + ":" + m.Column
}

// alpha is the family-wise significance level: it is Bonferroni-divided
// across the gated metrics (each metric is judged at alpha/m), otherwise a
// 20-metric gate would false-positive on most runs. The command warns when
// too few runs make the corrected level unreachable.
const alpha = 0.05

// metricColumns are the value columns of the bench CSV schema (csvHeader)
// that a metrics file may name.
//
//nolint:gochecknoglobals // fixed label set, read-only
var metricColumns = strings.Split(csvHeader, ",")[1:]

// parseMetricsFile reads the gated-metric list: one
// "<csv path>,<row>,<column>" per line, blank lines and #-comments ignored.
func parseMetricsFile(path string) ([]metricSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open metrics file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var specs []metricSpec
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.Split(text, ",")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s:%d: expected <csv path>,<row>,<column>, got %q", path, line, text)
		}
		spec := metricSpec{
			File:   strings.TrimSpace(parts[0]),
			Row:    strings.TrimSpace(parts[1]),
			Column: strings.TrimSpace(parts[2]),
		}
		if !slices.Contains(metricColumns, spec.Column) {
			return nil, fmt.Errorf("%s:%d: unknown column %q (expected one of %s)",
				path, line, spec.Column, strings.Join(metricColumns, ","))
		}
		specs = append(specs, spec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read metrics file: %w", err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s: no metrics listed", path)
	}
	return specs, nil
}

// readMetric extracts one spec's value from the CSVs under one run root. A
// missing file, row, or unparsable value is an error: a gated metric that
// silently disappears must fail the comparison, not pass it.
func readMetric(root string, spec metricSpec) (float64, error) {
	path := filepath.Join(root, filepath.FromSlash(spec.File))
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("metric %s: %w", spec, err)
	}
	defer func() { _ = f.Close() }()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return 0, fmt.Errorf("metric %s: reading %s: %w", spec, path, err)
	}
	if len(records) == 0 {
		return 0, fmt.Errorf("metric %s: %s is empty", spec, path)
	}
	col := slices.Index(records[0], spec.Column)
	if col < 0 {
		return 0, fmt.Errorf("metric %s: %s has no %q column", spec, path, spec.Column)
	}
	for _, rec := range records[1:] {
		if rec[0] != spec.Row {
			continue
		}
		v, perr := strconv.ParseFloat(rec[col], 64)
		if perr != nil {
			return 0, fmt.Errorf("metric %s: value %q: %w", spec, rec[col], perr)
		}
		if v <= 0 {
			return 0, fmt.Errorf("metric %s: non-positive value %v", spec, v)
		}
		return v, nil
	}
	return 0, fmt.Errorf("metric %s: row %q not found in %s", spec, spec.Row, path)
}

// verdict is one gated metric's outcome.
type verdict int

const (
	verdictOK verdict = iota
	verdictImprovement
	verdictRegression
	verdictUnstable
)

func (v verdict) String() string {
	switch v {
	case verdictImprovement:
		return "improvement"
	case verdictRegression:
		return "REGRESSION"
	case verdictUnstable:
		return "unstable (not gated)"
	default:
		return "ok"
	}
}

// comparison is one gated metric's full result.
type comparison struct {
	Spec     metricSpec
	BaseMean float64
	CandMean float64
	Delta    float64 // (candidate − baseline) / baseline, of the means
	PValue   float64 // exact permutation p-value of that delta
	Noise    float64 // worse side's within-side relative spread
	Verdict  verdict
}

// compareMetrics reads every gated metric from every run root and judges each
// one. It fails on the first unreadable metric.
func compareMetrics(
	specs []metricSpec, baseRoots, candRoots []string, minEffect, maxNoise float64,
) ([]comparison, error) {
	sigLevel := alpha / float64(len(specs))
	comps := make([]comparison, 0, len(specs))
	for _, spec := range specs {
		base, err := readSide(baseRoots, spec, "baseline")
		if err != nil {
			return nil, err
		}
		cand, err := readSide(candRoots, spec, "candidate")
		if err != nil {
			return nil, err
		}
		c, err := compareOne(spec, base, cand, sigLevel, minEffect, maxNoise)
		if err != nil {
			return nil, err
		}
		comps = append(comps, c)
	}
	return comps, nil
}

func readSide(roots []string, spec metricSpec, side string) ([]float64, error) {
	values := make([]float64, 0, len(roots))
	for _, root := range roots {
		v, err := readMetric(root, spec)
		if err != nil {
			return nil, fmt.Errorf("%s run %s: %w", side, root, err)
		}
		values = append(values, v)
	}
	return values, nil
}

func compareOne(spec metricSpec, base, cand []float64, sigLevel, minEffect, maxNoise float64) (comparison, error) {
	pValue, err := permutationPValue(base, cand)
	if err != nil {
		return comparison{}, fmt.Errorf("metric %s: %w", spec, err)
	}
	c := comparison{
		Spec:     spec,
		BaseMean: mean(base),
		CandMean: mean(cand),
		PValue:   pValue,
		Noise:    math.Max(relSpread(base), relSpread(cand)),
	}
	c.Delta = (c.CandMean - c.BaseMean) / c.BaseMean
	switch {
	case c.Noise >= maxNoise:
		c.Verdict = verdictUnstable
	case c.PValue <= sigLevel && c.Delta >= minEffect:
		c.Verdict = verdictRegression
	case c.PValue <= sigLevel && c.Delta <= -minEffect:
		c.Verdict = verdictImprovement
	default:
		c.Verdict = verdictOK
	}
	return c, nil
}

func mean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// relSpread is one side's within-side relative spread — how much this
// benchmark varies between runs of the same build. With at least 4 runs the
// single highest and lowest are dropped first: one stray run should not
// declare the benchmark unstable (the significance test already discounts
// outliers), only chronic jitter should.
func relSpread(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	if len(sorted) >= 4 {
		sorted = sorted[1 : len(sorted)-1]
	}
	return (sorted[len(sorted)-1] - sorted[0]) / mean(values)
}

// maxSplits bounds the exact permutation enumeration; C(2K,K) stays far below
// it for any sane per-side run count (K=10 → 184,756).
const maxSplits = 500_000

// permutationPValue runs the exact two-sided permutation test: pool both
// sides' per-run values, enumerate every split of the pool into the original
// group sizes, and return the fraction of splits whose |relative mean
// difference| is at least the observed one. A small value means shuffling the
// build labels almost never explains a difference this large.
func permutationPValue(base, cand []float64) (float64, error) {
	pool := slices.Concat(base, cand)
	n, k := len(pool), len(cand)
	splits := binomial(n, k)
	if splits == 0 || splits > maxSplits {
		return 0, fmt.Errorf("%d runs is too many for the exact permutation test", n)
	}
	observed := math.Abs((mean(cand) - mean(base)) / mean(base))

	inCand := make([]bool, n)
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	asExtreme := 0
	for {
		if math.Abs(splitDiff(pool, inCand, idx)) >= observed {
			asExtreme++
		}
		// Advance idx to the next k-combination of {0..n-1}.
		i := k - 1
		for i >= 0 && idx[i] == n-k+i {
			i--
		}
		if i < 0 {
			break
		}
		idx[i]++
		for j := i + 1; j < k; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
	return float64(asExtreme) / float64(splits), nil
}

// splitDiff computes the relative mean difference of one pool split: the
// values at idx play candidate, the rest play baseline. inCand is scratch
// space (fully overwritten each call).
func splitDiff(pool []float64, inCand []bool, idx []int) float64 {
	for i := range inCand {
		inCand[i] = false
	}
	for _, i := range idx {
		inCand[i] = true
	}
	var candSum, baseSum float64
	for i, v := range pool {
		if inCand[i] {
			candSum += v
		} else {
			baseSum += v
		}
	}
	candMean := candSum / float64(len(idx))
	baseMean := baseSum / float64(len(pool)-len(idx))
	return (candMean - baseMean) / baseMean
}

// binomial returns C(n, k), or 0 when it overflows past any split count we
// accept.
func binomial(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := 1; i <= k; i++ {
		result = result * (n - k + i) / i
		if result > maxSplits*2 {
			return 0
		}
	}
	return result
}

// regressions returns the specs that failed the gate.
func regressions(comps []comparison) []string {
	var out []string
	for _, c := range comps {
		if c.Verdict == verdictRegression {
			out = append(out, c.Spec.String())
		}
	}
	return out
}

// formatValue renders a metric value: duration columns as durations,
// count columns as plain numbers.
func formatValue(spec metricSpec, v float64) string {
	if strings.HasSuffix(spec.Column, "_ns") {
		return time.Duration(v).Round(time.Microsecond).String()
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// writeSummaryMarkdown renders the comparison table (GitHub-flavored
// Markdown, suitable for a job summary).
func writeSummaryMarkdown(w io.Writer, comps []comparison, baseRuns, candRuns int) error {
	if _, err := fmt.Fprintf(w,
		"### Full-history benchmark comparison\n\n"+
			"%d baseline / %d candidate runs, interleaved on one machine; per-side means "+
			"compared with an exact permutation test (family-wise alpha %.2f over %d "+
			"gated metrics → per-metric %.4f).\n\n"+
			"| metric | baseline | candidate | delta | p | noise | verdict |\n"+
			"|---|---:|---:|---:|---:|---:|---|\n",
		baseRuns, candRuns, alpha, len(comps), alpha/float64(len(comps))); err != nil {
		return err
	}
	for _, c := range comps {
		if _, err := fmt.Fprintf(w, "| %s | %s | %s | %+.1f%% | %.3f | %.1f%% | %s |\n",
			c.Spec, formatValue(c.Spec, c.BaseMean), formatValue(c.Spec, c.CandMean),
			100*c.Delta, c.PValue, 100*c.Noise, c.Verdict); err != nil {
			return err
		}
	}
	return nil
}
