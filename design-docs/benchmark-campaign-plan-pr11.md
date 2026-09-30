# PR 11: Remove the CSV output of `bench-ingest`

| | |
|---|---|
| Branch | `bench-campaign-v2/11-remove-csv` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 03 (`results.json` writer), benchmarks B1 (the converter reads `results.json`) merged and deployed. D12, D34. PR 12 (`freeze`) if it merged first: it reuses the same sink. |
| Implements | D12 (last sentence: remove the CSV files), D34 (`<group>.<row>` names stay after the CSV writers go); spec 6.1 item 2, 7.1 |
| Estimate | About 170 changed non-test lines (`git diff --stat`, insertions plus deletions): `csvsink.go` → `sink.go` about 110 (70 removed, 40 renamed), `command.go` 20, `cold.go` 12, `hot.go` 12, `rss.go` 10, `doc.go` 6 |

## 1. Goal

After this PR, `bench-ingest cold` and `bench-ingest hot` write no CSV file.
`results.json` (PR 03) is the only results file of a bench command.
The in-memory aggregation stays, because `results.json` is built from it.
The benchmarks converter tests still pass.

## 2. Scope

- Rename `P/bench/csvsink.go` to `P/bench/sink.go` and the type `csvSink` to `sink`. Remove the CSV writers.
- Modify `P/bench/command.go`: remove `writePartialCSVs`; change the `--out` help text.
- Modify `P/bench/cold.go` and `P/bench/hot.go`: remove the `writeCSVs` calls and the "wrote %d CSVs" log lines.
- Modify `P/bench/rss.go`: record the peak RSS as a value, not as a duration sample.
- Modify `P/bench/doc.go`, the bench schema README (PR 03), and spec 7.1 (drop "Until PR 11 ...").
- Update the tests in `P/bench` that read CSV files.

## 3. Out of scope (boundaries)

- The `results.json` schema and its writer: PR 03. This PR changes no field and does not increase `schemaVersion` (D29): no reader of `results.json` sees a change.
- The converter change that reads `results.json`: benchmarks B1.
- Removing CSV support from the converter: a later benchmarks change. The converter keeps reading old bundles in `docs/runs/`.
- `invocation.json`: PR 03 decides whether it stays. This PR does not touch it.
- `bench-query` CSV rows (`<qtype>_r<rate>_millirps`): they exist only on the #12 branch, which D1 drops.

## 4. Design

### 4.1 What stays in `sink.go`

These parts feed `results.json`. Keep them and their behavior:
- `sample`, `series`, `series.observe`.
- `rowKey{file, row string}` → rename to `rowKey{group, row string}`. The group is the old CSV file stem: `ledgers`, `txhash`, `events`, `hot`, `driver`. D34 names each measurement `<group>.<row>`, for example `driver.ingest_total` and `ledgers.write`. The key must keep the group, because `write` and `finalize` occur in three groups.
- `fileSpec`, `fileSpecs` → rename to `groupSpec`, `groupSpecs`. Keep the row order: `results.json` lists the measurements in this order.
- The row constants (`driverBackfillWall`, `driverIndexRebuild`, `driverChunkTotal`, `driverTotalSuffix`, `driverColdExtract`, `driverIngestTotal`, `driverRunWall`, `driverPaceLag`, `coldType*`, `coldStage*`), `fileHot`, `fileDriver` → `groupHot`, `groupDriver`.
- Every `ingest.MetricSink` and `observability.Metrics` method (`HotPhase`, `ColdIngest`, `ColdChunkTotal`, `ColdExtract`, `IngestStage`, `Freeze`, `Rebuild`, `LastCommitted` and the no-op methods), `recordPaceLag`, `lastCommittedSeq`, `sumDriver`.
- `row`, `aggregate` (the zero filter and `includeZeros` for `pace_lag`), `withUnknown`.
- `file`, `files()` → rename to `group`, `groups()`. PR 03's writer reads them.
- `logSummary`: keep; it logs one line per row.

### 4.2 What goes

- `csvHeader`.
- `(*csvSink).writeCSVs` and `writeCSV`.
- `writePartialCSVs` in `command.go`. A failed run already writes `results.json` with `status: failed` and the partial measurements (PR 03, spec 7.2 rule 7). Check that PR 03 does this before you remove the function.
- The calls in `cold.go` (`runCold`: `writePartialCSVs` on the error path, `sink.writeCSVs(opts.OutDir)`, `logger.Infof("wrote %d CSVs ...")`) and the same three in `hot.go` (`runHot`).
- The comments that name CSV: `cold.go` `coldOptions.OutDir` ("receives the CSV report"), `hot.go` `hotOptions.OutDir`, `runCold` and `runHot` doc comments, `doc.go` ("aggregates each run into percentile CSV reports").
- `--out` help text: from "output dir for the CSV report and invocation.json" to "output dir for results.json" (plus `invocation.json` if PR 03 keeps it).

### 4.3 Peak RSS

Today `recordPeakRSS` stores the byte count as a `time.Duration` sample on row `peak_rss_bytes` (`driverPeakRSS`). The comment says this lets the row use the CSV format. PR 03 maps the row to `driver.peak_rss` with `kind: value`, `unit: bytes`.
- After this PR the CSV format is gone. Store the value in a field: `sink.peakRSS uint64` and `sink.peakRSSKnown bool`, set by `recordPeakRSS`.
- Rename the constant to `driverPeakRSS = "peak_rss"`. Remove the `peak_rss_bytes` special case in `logSummary`.
- PR 03's writer reads the field instead of the series. The `results.json` output stays the same. `TestResultsPeakRSS` (Section 6) proves it.
- If this change goes over the size, leave it for a later PR. It is not required by D12.

### 4.4 Data flow after the PR

`backfill.RunBackfill` / the hot loop → `sink` methods → `sink.rows` → `sink.groups()` → PR 03 `writeResults(outDir, ...)` → `<out>/results.json`. No other file in `--out` except `invocation.json` (if kept) and the profile files of `--cpuprofile` and `--memprofile`.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `P/bench/csvsink.go` → `P/bench/sink.go` | rename, modify | remove writers, rename types, peak RSS field | 110 |
| `P/bench/command.go` | modify | remove `writePartialCSVs`, help text | 20 |
| `P/bench/cold.go` | modify | remove CSV calls and comments | 12 |
| `P/bench/hot.go` | modify | remove CSV calls and comments | 12 |
| `P/bench/rss.go` | modify | value field | 10 |
| `P/bench/doc.go` | modify | package comment | 6 |
| `P/bench/README.md` (PR 03) | modify | remove the CSV note (not counted) | — |
| `design-docs/benchmark-campaign.md` | modify | spec 6.1 item 2, 7.1 (not counted) | — |

Use `git mv` for the rename so `git diff --stat -M` counts only the changed lines.

## 6. Tests

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestRunColdWritesNoCSV` | `P/bench/bench_test.go` | A cold run writes `results.json` and no `*.csv` | Existing helper `writeSourcePack(t, root, chunk.ID(0), chunk.LedgersPerChunk)`; `runCold`; `filepath.Glob(out + "/*.csv")` is empty. |
| `TestRunHotWritesNoCSV` | `P/bench/bench_test.go` | The same for hot, also on a failed run | `writeSourcePack` with 200 ledgers; `TestRunHotIncompleteStream` setup for the failed case. |
| `TestSinkExactOutput` | `P/bench/sink_test.go` | Replaces `TestCSVSinkExactOutput`: the groups and rows equal the old CSV rows (same names, `n`, `n_items`, percentiles) | Assert on `sink.groups()`. Keep the old expected numbers. |
| `TestSinkPaceLag*`, `TestSinkHotIngestTotal`, `TestSinkEmpty`, `TestSinkLastCommitted` | `P/bench/sink_test.go` | The renamed `TestCSVSink*` tests keep their checks | Replace `mustWriteCSVs` with a helper `mustGroups(t, s) map[string]map[string]row`. |
| `TestResultsPeakRSS` | `P/bench/rss_test.go` | `driver.peak_rss` is `kind: value`, `unit: bytes`, same number | Replaces the two `writeCSVs` calls in `rss_test.go`; read `results.json` with PR 03's reader helper. |
| Existing `TestRunColdFromPack`, `TestRunColdMultiChunk`, `TestRunHotFromPack`, `TestRunHotPaced` | `P/bench/bench_test.go` | Same checks through `results.json` | Replace the 10 `readCSV` calls with PR 03's results reader (if PR 03 adds none, add `readResults(t, dir) map[string]measurement` keyed by `<group>.<row>`). Remove `readCSV`. |
| `TestCampaignEndToEnd` (PR 07) | `P/bench/campaign_e2e_test.go` | Step directories hold no `*.csv` | Add one glob check. |
| Converter tests | stellar-rpc-benchmarks `converter/tests` | The converter still passes on its own tests and converts a bundle without CSV files | `make test` (`python3 -m unittest discover converter/tests`) and `make smoke` at the B1 commit. Then convert a bundle from `TestCampaignEndToEnd` (keep it with `BENCH_CAMPAIGN_KEEP=<dir>`) with the B1 converter command. |

## 7. Done when

- `bench-ingest` writes no CSV file: `go test ./cmd/stellar-rpc/internal/rpcv2/bench -run 'TestRunColdWritesNoCSV|TestRunHotWritesNoCSV'`.
- The converter tests still pass: in stellar-rpc-benchmarks at the B1 commit, `make test` and `make smoke` pass, and the B1 converter converts the kept e2e bundle with no error.
- `git grep -n -i csv -- cmd/stellar-rpc/internal/rpcv2/bench ':!*_test.go'` prints nothing.

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
make go-check-branch BASE=feature/full-history
git diff --stat -M feature/full-history -- . ':!*_test.go' ':!*.md'
# in stellar-rpc-benchmarks at the B1 commit
make test && make smoke
```

## 9. Risks and open points

- Check first that B1 is merged and deployed. The converter at `3bc7c49` reads only CSV (`convert.py` matches `driver.csv` rows by name and reads `peak_rss_bytes` from `total_ns`). A bundle without CSV fails there.
- Check that B1 reads the D34 names (`driver.ingest_total`, `ledgers.write`). The converter today derives `ledgers_per_s` from `*_total.n_items`; `results.json` must carry `items` (spec 7.2 rule 4).
- The shape of PR 03's writer is not known yet. This plan assumes that PR 03 builds `results.json` from `sink.files()`. If PR 03 builds it another way, keep what that writer reads and remove the rest.
- The rename `csvSink` → `sink` touches every method receiver. With `git mv` and `-M`, the diff counts only the lines that change. If a reviewer wants a smaller diff, keep the type name and change only the file name and the comments.
- No other file in stellar-rpc at `91f158b` reads the CSV files (`git grep` for `.csv` in scripts, workflows and docs is empty). The old box scripts on the #15 to #20 branches do not read them either.
- Spec 7.1 keeps the line "Until PR 11, the ingest step directories also hold the CSV files". Remove it in this PR. Add a decision-log note under D12 that the removal is done.
