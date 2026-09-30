# PR 03: `results.json` and `--paced-ledgers` in `bench-ingest`

| | |
|---|---|
| Branch | `bench-campaign-v2/03-ingest-results` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | None in code. Same files as PR 02 (`cold.go`, `hot.go`, `command.go`): rebase on PR 02 if it merges first. Decisions D1 (no reuse of #12 code), D12, D21, D29, D34 (measurement names). |
| Implements | D12, D21, D29, D34; spec Section 6.1 items 2 to 4 (item 4 in the README only; the runner, PR 07, passes the flags), Section 7.2 |
| Estimate | About 270 non-test lines: `bench/results.go` 110, `bench/csvsink.go` 45, `bench/pace.go` 25, `bench/hot.go` 45, `bench/cold.go` 25, `bench/command.go` 20 |

`P` is `cmd/stellar-rpc/internal/rpcv2`. All identifiers below were read at `91f158b`.

## 1. Goal

After this PR, each `bench-ingest cold` and `bench-ingest hot` run writes `<out>/results.json` with the schema of spec Section 7.2 and D34. The command writes it with `status: running` at start and again at the end; each write is a temporary file and a rename. `bench-ingest hot` has `--paced-ledgers N`: only the last N ledgers are paced and measured. The CSV files and `invocation.json` stay.

## 2. Scope

- New `P/bench/results.go`: the record types and the start and end writes.
- `P/bench/csvsink.go`: `measurements()` maps the aggregated rows to measurements; the gate that leaves unpaced ledgers out.
- `P/bench/pace.go` and `P/bench/hot.go`: `--paced-ledgers` (skip count in the stream wrapper, `run_wall` from the first measured ledger).
- `P/bench/cold.go`, `P/bench/command.go`: call sites, parameters, the raw flag map, the new flag.
- New `P/bench/README.md`: the `results.json` schema.

## 3. Out of scope (boundaries)

- Removal of the CSV files and of `invocation.json`: PR 11 (after B1). This PR changes neither.
- The converter that reads `results.json`: B1 in the benchmarks repository.
- The link from `cmd/bench-campaign/README.md` to the schema README: PR 06 creates that file.
- Campaign flags (`--source pack --pack-dir <root>/<profile>/packs`, `--close-interval` always set, spec 6.1 item 4): the runner, PR 07, passes them. This PR only lists them in the README.
- `results.json` for `freeze`: PR 12 (uses this writer with `command: freeze`).
- Code from #12 (`bench-query/01-dispatcher-schema`, `invocation.go`): D1 allows the pattern only (temporary file, rename, `status`). Write the code new.

## 4. Design

### 4.1 Record (`results.go`)

```go
type resultsRecord struct {
    SchemaVersion int            `json:"schemaVersion"`   // 1 (D29)
    Command       string         `json:"command"`         // ingest-cold | ingest-hot | freeze
    Binary        resultsBinary  `json:"binary"`          // {version, commit, buildTimestamp}
    Hostname      string         `json:"hostname"`        // os.Hostname()
    StartedAt     string         `json:"startedAt"`       // RFC 3339, UTC
    FinishedAt    string         `json:"finishedAt"`      // "" while running
    Status        string         `json:"status"`          // running | ok | failed
    Error         string         `json:"error"`
    Parameters    map[string]any `json:"parameters"`      // spec 7.2 rule 9 keys and "flags"
    Measurements  []measurement  `json:"measurements"`
}
```

`resultsBinary{Version: version.Version, Commit: version.CommitHash, BuildTimestamp: version.BuildTimestamp}`. The Makefile ldflags set `version.CommitHash` from `git rev-parse HEAD`, so it is the full hash, and set `version.BuildTimestamp` from `BUILD_TIMESTAMP`. A plain `go build` leaves both empty; the README says so. `branch` goes (D12). `Hostname` is `os.Hostname()`, empty on error, as `writeInvocationJSON` does.

```go
type measurement struct {
    Name  string   `json:"name"`
    Kind  string   `json:"kind"`             // summary | value
    Unit  string   `json:"unit"`             // ns | bytes | count
    Count *int64   `json:"count,omitempty"`  // summary only
    Items *int64   `json:"items,omitempty"`
    P50, P90, P99, Max, Total *int64          // summary only; json p50 p90 p99 max total
    Value *int64   `json:"value,omitempty"`  // value only
}
```

All values are integers (spec 7.2 rule 1). Pointers keep a zero `items` in the output for a summary and keep summary fields out of a value.

### 4.2 Writer

```go
type resultsWriter struct { path string; rec resultsRecord }
func newResultsWriter(outDir, command string, params map[string]any, flags map[string]string) *resultsWriter
func (w *resultsWriter) start() error                                  // status running
func (w *resultsWriter) finish(m []measurement, runErr error) error    // status ok|failed, finishedAt, error
func writeJSONAtomic(path string, v any) error
```

`writeJSONAtomic`: `json.MarshalIndent`, `os.CreateTemp(dir, ".results-*.json.tmp")`, write, `f.Sync()`, `f.Close()`, `os.Rename`. One deferred `os.Remove(tmp)` is the one cleanup path (a no-op after the rename). A kill between the create and the rename leaves a hidden temporary file and the previous `results.json`; the next write does not need to remove it.

Call sites, in `runCold` and `runHot`, after `validate`, the dataset check (PR 02) and `os.MkdirAll(opts.OutDir)`:

```go
res := newResultsWriter(opts.OutDir, "ingest-hot", opts.parameters(), flags)
if err := res.start(); err != nil { return err }
defer func() { err = errors.Join(err, res.finish(sink.measurements(), err)) }()
```

`err` is the named return. `finish` uses the partial sink on a failed run, as `writePartialCSVs` does. Validation errors return before `start`, so `TestBenchRejectsInvalidSourceEarly` (no directory created) stays green. `newBenchCommand` (`command.go`) passes `captureFlags(cmd)` to `run`, so the `run` callback gets a `flags map[string]string` argument. `newResultsWriter` stores it as `parameters.flags`. `newBenchCommand` keeps `writeInvocationJSON`.

### 4.3 Measurements (D34)

`(*csvSink).measurements() []measurement` walks `s.files()`, the same aggregation that `writeCSVs` uses, so zero-duration filtering and the `pace_lag` zero rule are identical to the CSV.

Name = `<group>.<row>` (spec 7.2 rule 2). Group = the CSV file stem (`driver`, `hot`, `ledgers`, `txhash`, `events`). Row = the CSV row name without a unit suffix. The only suffix today is `_bytes` on `peak_rss_bytes` (`driverPeakRSS`). Reason (D34): `write` and `finalize` occur in `ledgers.csv`, `txhash.csv` and `events.csv`, so the row alone is not a key.

| CSV (file, row) | Measurement | Kind | Unit | `count` / `items` |
|---|---|---|---|---|
| `driver`, `backfill_wall`, `index_rebuild`, `chunk_total` (cold) | `driver.backfill_wall` ... | summary | ns | `n` / `n_items` (0) |
| `driver`, `ledgers_total`, `txhash_total`, `events_total` (cold) | `driver.ledgers_total` ... | summary | ns | `n` / engine item counts |
| `driver`, `cold_extract` (cold) | `driver.cold_extract` | summary | ns | `n` / `n_items` |
| `driver`, `ingest_total` (hot) | `driver.ingest_total` | summary | ns | paced ledgers / paced ledgers |
| `driver`, `run_wall` (hot) | `driver.run_wall` | summary | ns | 1 / paced ledgers |
| `driver`, `pace_lag` (hot, `--close-interval > 0`) | `driver.pace_lag` | summary | ns | paced ledgers / paced ledgers |
| `driver`, `peak_rss_bytes` (both) | `driver.peak_rss` | value | bytes | none; `value` = the byte count |
| `hot`, `extract`, `ledgers`, `txhash`, `events`, `commit`, `apply` | `hot.extract` ... | summary | ns | `n` / `n_items` |
| `ledgers`/`txhash`/`events`, `term_index`, `write`, `finalize` (cold) | `ledgers.write` ... | summary | ns | `n` / `n_items` |
| any row outside the schema (`withUnknown`) | `<file>.<row>` | summary | ns | as CSV |

Summary values: `p50`, `p90`, `p99`, `max`, `total` in integer nanoseconds, `d.Nanoseconds()`. `peak_rss` value: `r.total.Nanoseconds()` (the CSV stores bytes in the duration fields, `recordPeakRSS`). No row of `bench-ingest` has `unit: count` today; the README lists the unit for later rows. The converter converts units (B1).

### 4.4 Parameters

| Command | Keys |
|---|---|
| `ingest-cold` | `startChunk`, `numChunks`, `workers`, `source` (`pack`/`bsb`), `txhashIndexChunks` |
| `ingest-hot` | `startChunk`, `numChunks`, `numLedgers`, `closeInterval` (Go duration string), `pacedLedgers`, `unpacedLedgers`, `source` |
| both | `flags`: the raw flag map of `captureFlags` (flag name to string value, all flags) |

`txhashIndexChunks` (spec 7.2 rule 9): after `RunBackfill`, sum `Hi - Lo + 1` over the `frozen` entries of `cat.AllTxHashIndexKeys()`. `finish` gets it through `res.rec.Parameters` before the deferred call runs.

No other key. `bench-ingest` has no pack-format notion (`packBackend` reads `{bucket:05d}/{chunk:08d}.pack` only), and `campaign.json` carries the packs prefix.

### 4.5 `--paced-ledgers` (D21)

Flag in `newHotCommand`: `fs.Uint32Var(&pacedLedgers, "paced-ledgers", 10000, "...")`. New field `hotOptions.PacedLedgers uint32`.

Range: `first` and `last` as today (`--num-ledgers` still caps `last`). `total := last - first + 1`. `hotOptions.validate` returns an error when `PacedLedgers > total` (spec 6.1 item 3, D21), so the run fails before `start` and writes no `results.json`. `paced := total` when `PacedLedgers == 0`; else `paced := PacedLedgers`. `unpaced := total - paced`. `parameters` records both values.

Stream (`pace.go`, `hot.go`):

- `pacingStream` gets a field `skip int`. The first `skip` ledgers yield with no wait and no schedule call. Then `due := p.schedule.dueForPos(pos - skip)`.
- `buildHotStream(backend, first, last, closeInterval, unpaced)` always returns a `pacingStream` and its schedule: `newPaceSchedule(closeInterval, first+unpaced)`. With `closeInterval == 0` every wait is non-positive, so `contextSleep` returns at once. The schedule then only records the anchor.
- `sink.schedule` is set only when `closeInterval > 0`, as today. `recordPaceLag` uses `dueForSeq`, which returns false for `seq < firstSeq`, so unpaced ledgers record no `pace_lag`.

Sink gate (`csvsink.go`): add `unmeasured int` and `applied int` (under `mu`). In `HotPhase`: `measured := s.applied >= s.unmeasured`. Observe the `hot` row and `ingest_total` only when `measured`. On `hotchunk.PhaseApply`, increment `applied`. This works because the loop is one goroutine and each committed ledger ends with exactly one `PhaseApply` (`ingest/service.go`). All `hot.*` rows cover the paced ledgers only (spec 6.1 item 3, D21), the same as `ingest_total`; the README says so.

`run_wall`: `sink.observe(fileDriver, driverRunWall, time.Since(anchor), int(paced))`, where `anchor` comes from a new `(*paceSchedule).anchorTime() (time.Time, bool)`. Today `run_wall` starts before `RunBoundedIngestionLoop`; now it starts at the first measured yield. With `unpaced == 0` the difference is the source open and the first read.

Example:

```
stellar-rpc-v2 bench-ingest hot --source pack --pack-dir /mnt/nvme/bench/sac-6000/packs \
  --start-chunk 1 --num-chunks 2 --close-interval 2s --paced-ledgers 10000 \
  --hot-dir /mnt/nvme/bench/sac-6000/run1/hot --out <step dir>
# ledgers 10002..20001 ingest back to back; 20002..30001 are paced and measured
```

### 4.6 Schema README (`P/bench/README.md`)

Sections: file location and write rules (start, end, rename, `status`); top-level fields; `parameters` per command; the measurement table of 4.3 with each name, kind, unit and the ledgers it covers; `schemaVersion` rule (D29: increase on each incompatible change); the note that `binary.commit` and `binary.buildTimestamp` are empty for a binary built without the Makefile ldflags; the campaign flags of spec 6.1 item 4. The Blaster field list (spec 7.4) is added by PR 10.

## 5. Files

| File | Change | What | Non-test lines |
|---|---|---|---|
| `P/bench/results.go` | new | `resultsRecord`, `measurement`, `resultsWriter`, `writeJSONAtomic` | 110 |
| `P/bench/csvsink.go` | modify | `measurements()`, `unmeasured`/`applied` gate in `HotPhase` | 45 |
| `P/bench/pace.go` | modify | `pacingStream.skip`, `anchorTime` | 25 |
| `P/bench/hot.go` | modify | `PacedLedgers`, range split, `buildHotStream`, `run_wall`, results calls, `parameters()` | 45 |
| `P/bench/cold.go` | modify | results calls, `parameters()`, `txhashIndexChunks` | 25 |
| `P/bench/command.go` | modify | `--paced-ledgers` flag and help; `flags` to `run`; `--out` help mentions `results.json` | 20 |
| `P/bench/README.md` | new | schema | not counted |
| `P/bench/results_test.go` | new | writer and schema tests | test |
| `P/bench/bench_test.go`, `pace_test.go`, `csvsink_test.go` | modify | run tests | test |

## 6. Tests

Helpers: `writeSourcePack`, `testLogger`, `readCSV` (`bench_test.go`); `fakeClock`, `staticStream` (`pace_test.go`). New helper `readResults(t, outDir) resultsRecord` and `measurementByName(t, rec, name) measurement`.

| Test | Package/file | Proves | How |
|---|---|---|---|
| `TestWriteJSONAtomic` | `bench`, `results_test.go` | The rename replaces the file; no temporary file remains. | Two writes; one `results.json`, no `.results-*` entry. |
| `TestResultsWriterStartFinish` | same | `start` writes `running` with empty `finishedAt`; `finish(nil)` writes `ok`; `finish(err)` writes `failed` and the message. | Direct calls in `t.TempDir()`. |
| `TestMeasurementsMapping` | `bench`, `csvsink_test.go` | Names `<group>.<row>`, `peak_rss` as value/bytes, integer `ns` values, `items` kept, zero filter same as CSV. | Feed a `csvSink` with known samples (as `TestCSVSinkExactOutput` does); compare with the CSV row values. |
| `TestRunColdWritesResults` | `bench`, `bench_test.go` | A cold run writes a record that matches the README. | 1 chunk from `writeSourcePack`; assert `command`, `status: ok`, `binary` (three keys), `hostname`, `parameters` (incl. `txhashIndexChunks: 1` and `flags`), `driver.backfill_wall`, `driver.ledgers_total.items == 10000`, `ledgers.write`; on Linux `driver.peak_rss` value > 0. |
| `TestRunHotWritesResults` | same | A hot run writes a record that matches the README. | 200 ledgers; `pacedLedgers: 200`, `unpacedLedgers: 0`; `driver.ingest_total.count == 200`; `hot.commit`. |
| `TestRunHotPacedLedgers` | same | Spec: with `--paced-ledgers 100`, `driver.ingest_total` has 100 samples. | 150 ledgers, `PacedLedgers: 100`, `CloseInterval: 5ms`; `driver.ingest_total.count == 100`, `driver.pace_lag.items == 100`, `driver.run_wall.items == 100`, `hot.commit.count == 100`; `parameters.unpacedLedgers == 50`; wall time ≥ 99 × 5 ms. |
| `TestRunHotPacedLedgersAboveRange` | same | A value above the range is an error. | 50 ledgers, `PacedLedgers: 1000`; `runHot` returns an error; no `results.json`. `PacedLedgers: 50` runs and records `unpacedLedgers: 0`. |
| `TestRunHotLeavesRunningWhenKilled` | same | Spec: a killed run leaves `status: running`. | Run `runHot` in a goroutine with 3 ledgers and `CloseInterval: time.Hour`. Poll until `results.json` exists; read `status == running` and empty `finishedAt`. That file is what a SIGKILL leaves. Then cancel the context; the final record says `failed`. |
| `TestRunFailedWritesFailed` | same | A failed run writes `failed` with the error and partial measurements. | Reuse the `TestRunHotIncompleteStream` setup. |
| `TestPacingStreamSkip` | `bench`, `pace_test.go` | Skipped ledgers yield with no wait; the schedule anchors at the first paced ledger. | `fakeClock`, `staticStream`, recording `sleep`. |
| existing CSV tests | `bench` | CSV output is unchanged. | `TestCSVSinkExactOutput` and the others pass. `TestRunHotPaced` still passes (20 ledgers < default 10,000). |

## 7. Done when

| Spec item | Check |
|---|---|
| A cold run and a hot run each write a `results.json` that matches the README. | `TestRunColdWritesResults`, `TestRunHotWritesResults`. The reviewer checks that each field in the tests is in `P/bench/README.md`. |
| A killed run leaves `status: running`. | `TestRunHotLeavesRunningWhenKilled`. |
| With `--paced-ledgers 100`, `driver.ingest_total` has 100 samples. | `TestRunHotPacedLedgers`. |
| CSV kept. | Existing `readCSV` assertions in `bench_test.go` pass unchanged. |

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'
make build-rpc-v2 && ./stellar-rpc-v2 bench-ingest hot --help | grep paced-ledgers
```

Manual check: run `bench-ingest hot` on a 150-ledger pack tree with `--paced-ledgers 100 --close-interval 10ms`, then `jq '.status, .parameters, [.measurements[].name]' <out>/results.json`.

## 9. Risks and open points

- D34 is the source of the `<group>.<row>` names (spec 7.2 rule 2). When PR 11 removes the CSV writers, the in-memory aggregation must keep the per-group key (`rowKey.file`).
- `version.BuildTimestamp` has the Makefile format (`date '+%Y-%m-%dT%H:%M:%S'`, local time, no zone). The record copies it as is; the README says so.
- `invocation.json` stays. PR 11 decides whether to remove it with the CSV files; the converter reads its `error` field today (facts A6).
- `run_wall` now starts at the first measured ledger. For a run with no unpaced ledgers the value is slightly smaller than before. B1 must not compare old and new `run_wall` without this note.
- The `HotPhase` gate counts `PhaseApply` calls. It depends on one committed ledger emitting one `PhaseApply`. A future change to `ingest/service.go` could break it; `TestRunHotPacedLedgers` catches that.
- Default `--paced-ledgers 10000` changes what the old benchmarks runner measures for hot runs longer than 10,000 ledgers. Its runs use one chunk (10,000 ledgers), so nothing changes today.
- The values change from CSV milliseconds to integer nanoseconds. B1 must read `unit` and convert; it must not reuse the CSV scale.
- `binary.commit` is empty for a binary built with plain `go build`. The runner (PR 07) must build with `make build-rpc-v2`.
