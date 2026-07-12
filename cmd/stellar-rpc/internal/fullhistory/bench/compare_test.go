package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMetricsFile writes a gated-metric list and returns its path.
func writeMetricsFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.csv")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	return path
}

// writeRunRoots materializes one CSV tree per value: run i holds
// sub/ledgers.csv with a single "write" row whose duration columns all carry
// values[i] nanoseconds. Returns the run roots.
func writeRunRoots(t *testing.T, dir string, values []float64) []string {
	t.Helper()
	roots := make([]string, len(values))
	for i, v := range values {
		root := filepath.Join(dir, fmt.Sprintf("run%d", i))
		path := filepath.Join(root, "sub", "ledgers.csv")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		ns := int64(v)
		content := fmt.Sprintf("%s\nwrite,5,100,%d,%d,%d,%d,%d\n", csvHeader, ns, ns, ns, ns, ns)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		roots[i] = root
	}
	return roots
}

var testSpec = metricSpec{File: "sub/ledgers.csv", Row: "write", Column: "total_ns"}

func TestParseMetricsFile(t *testing.T) {
	path := writeMetricsFile(t,
		"# gated metrics",
		"",
		"ingest-cold/ledgers.csv,write,total_ns",
		"  query-hot/events.csv , total_c1 , p50_ns  ",
	)
	specs, err := parseMetricsFile(path)
	require.NoError(t, err)
	require.Equal(t, []metricSpec{
		{File: "ingest-cold/ledgers.csv", Row: "write", Column: "total_ns"},
		{File: "query-hot/events.csv", Row: "total_c1", Column: "p50_ns"},
	}, specs)
}

func TestParseMetricsFileRejectsUnknownColumn(t *testing.T) {
	_, err := parseMetricsFile(writeMetricsFile(t, "a.csv,write,bogus_ns"))
	require.ErrorContains(t, err, "unknown column")
}

func TestParseMetricsFileRejectsShortLine(t *testing.T) {
	_, err := parseMetricsFile(writeMetricsFile(t, "a.csv,write"))
	require.ErrorContains(t, err, "expected <csv path>,<row>,<column>")
}

func TestParseMetricsFileRejectsEmpty(t *testing.T) {
	_, err := parseMetricsFile(writeMetricsFile(t, "# only a comment"))
	require.ErrorContains(t, err, "no metrics listed")
}

// TestCompareFlagsRegression: a clear slowdown well past the noise spread and
// the min-effect floor must come back as a regression.
func TestCompareFlagsRegression(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 101e6, 99e6, 100e6, 102e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{150e6, 151e6, 149e6, 150e6, 148e6})

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	require.Len(t, comps, 1)
	assert.Equal(t, verdictRegression, comps[0].Verdict)
	assert.InDelta(t, 0.50, comps[0].Delta, 0.03)
	assert.Equal(t, []string{testSpec.String()}, regressions(comps))
}

// TestCompareWithinNoisePasses: a candidate drawn from the same distribution
// as the baseline must not be flagged.
func TestCompareWithinNoisePasses(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 101e6, 99e6, 100e6, 102e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{101e6, 100e6, 102e6, 99e6, 100e6})

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	assert.Equal(t, verdictOK, comps[0].Verdict)
	assert.Empty(t, regressions(comps))
}

// TestCompareBelowMinEffectPasses: a uniform +3% is statistically clear but
// below the 5% min-effect floor — too small to matter, not gated.
func TestCompareBelowMinEffectPasses(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 100e6, 100e6, 100e6, 100e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{103e6, 103e6, 103e6, 103e6, 103e6})

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	assert.Equal(t, verdictOK, comps[0].Verdict)
}

// TestCompareUnstableNotGated: when the runs' own spread exceeds max-noise,
// the metric is reported unstable and never fails — even with a huge delta.
func TestCompareUnstableNotGated(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 300e6, 100e6, 300e6, 100e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{300e6, 100e6, 300e6, 100e6, 300e6})

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	assert.Equal(t, verdictUnstable, comps[0].Verdict)
	assert.Empty(t, regressions(comps))
}

// TestCompareImprovement: a clear speedup is reported as an improvement, not
// a failure.
func TestCompareImprovement(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{200e6, 201e6, 199e6, 200e6, 202e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{100e6, 101e6, 99e6, 100e6, 102e6})

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	assert.Equal(t, verdictImprovement, comps[0].Verdict)
	assert.Empty(t, regressions(comps))
}

// TestCompareBonferroniAcrossMetrics: each metric is judged at alpha/m. A
// borderline p (0.05 exactly, from 3v3 full separation) fires when it is the
// only gated metric but must not when a second metric shares the gate.
func TestCompareBonferroniAcrossMetrics(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 101e6, 99e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{150e6, 151e6, 149e6})
	second := metricSpec{File: testSpec.File, Row: testSpec.Row, Column: "p50_ns"}

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	assert.Equal(t, verdictRegression, comps[0].Verdict, "alone: p=0.05 fires at level 0.05")

	comps, err = compareMetrics([]metricSpec{testSpec, second}, base, cand, 0.05, 0.10)
	require.NoError(t, err)
	assert.Equal(t, verdictOK, comps[0].Verdict, "with m=2: p=0.05 > 0.025, suppressed")
	assert.Equal(t, verdictOK, comps[1].Verdict)
	assert.Empty(t, regressions(comps))
}

// TestCompareMissingRowErrors: a gated metric that disappeared must fail the
// comparison loudly, naming the metric, instead of passing silently.
func TestCompareMissingRowErrors(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{100e6})
	missing := metricSpec{File: "sub/ledgers.csv", Row: "no_such_stage", Column: "total_ns"}

	_, err := compareMetrics([]metricSpec{missing}, base, cand, 0.05, 0.10)
	require.ErrorContains(t, err, "no_such_stage")
}

// TestCompareOutlierRunNotSignificant: one wild candidate run inflates the
// mean, but the permutation test sees that reshuffling the labels explains it
// — no regression.
func TestCompareOutlierRunNotSignificant(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 100e6, 100e6, 100e6, 100e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{100e6, 100e6, 100e6, 100e6, 300e6})

	comps, err := compareMetrics([]metricSpec{testSpec}, base, cand, 0.05, 10.0)
	require.NoError(t, err)
	assert.Equal(t, verdictOK, comps[0].Verdict)
	assert.Greater(t, comps[0].PValue, alpha)
}

func TestPermutationPValue(t *testing.T) {
	same := []float64{100, 100, 100}
	p, err := permutationPValue(same, same)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, p, 1e-9, "identical sides: every split is as extreme")

	p, err = permutationPValue([]float64{99, 100, 101, 100, 100}, []float64{149, 150, 151, 150, 150})
	require.NoError(t, err)
	assert.InDelta(t, 1.0/252, p, 1e-9, "full separation: only the true labeling is as extreme")
}

func TestRelSpread(t *testing.T) {
	assert.InDelta(t, 0, relSpread([]float64{100, 100}), 1e-9)
	assert.InDelta(t, 1.0, relSpread([]float64{50, 150}), 1e-9)
	// With ≥4 runs the extremes are trimmed: one stray run is not jitter.
	assert.InDelta(t, 0, relSpread([]float64{100, 100, 100, 100, 300}), 1e-9)
}

func TestWriteSummaryMarkdown(t *testing.T) {
	comps := []comparison{{
		Spec:     testSpec,
		BaseMean: 100e6,
		CandMean: 150e6,
		Delta:    0.5,
		PValue:   0.004,
		Noise:    0.012,
		Verdict:  verdictRegression,
	}}
	var sb strings.Builder
	require.NoError(t, writeSummaryMarkdown(&sb, comps, 5, 5))
	out := sb.String()
	assert.Contains(t, out, "| sub/ledgers.csv:write:total_ns | 100ms | 150ms | +50.0% | 0.004 | 1.2% | REGRESSION |")
	assert.Contains(t, out, "5 baseline / 5 candidate runs")
}

// TestCompareCommandEndToEnd drives the cobra command: a regression must
// surface as a non-zero exit naming the metric, and the summary file must
// hold the rendered table.
func TestCompareCommandEndToEnd(t *testing.T) {
	base := writeRunRoots(t, t.TempDir(), []float64{100e6, 101e6, 99e6})
	cand := writeRunRoots(t, t.TempDir(), []float64{150e6, 151e6, 149e6})
	metrics := writeMetricsFile(t, "sub/ledgers.csv,write,total_ns")
	summary := filepath.Join(t.TempDir(), "summary.md")

	cmd := NewCompareCommand()
	args := make([]string, 0, 4+2*len(base)+2*len(cand))
	args = append(args, "--metrics-file", metrics, "--summary", summary)
	for _, r := range base {
		args = append(args, "--baseline-root", r)
	}
	for _, r := range cand {
		args = append(args, "--candidate-root", r)
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	require.ErrorContains(t, err, "performance regression in 1 metric(s)")
	require.ErrorContains(t, err, testSpec.String())

	content, rerr := os.ReadFile(summary)
	require.NoError(t, rerr)
	assert.Contains(t, string(content), "REGRESSION")
}
