# PR 02: `bench-ingest` keeps its catalog

| | |
|---|---|
| Branch | `bench-campaign-v2/02-ingest-catalog` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | None in code. D4 (confirmed), D14 (confirmed). PR 03 edits the same files (`cold.go`, `hot.go`, `command.go`): merge PR 02 first or rebase PR 03 on it. |
| Implements | D4, D14 (enforced by the existence check); spec Section 6.1 item 1; Q6 (answered yes) |
| Estimate | About 130 non-test lines (added plus removed): `bench/dataset.go` (replaces `scratch.go`) 60, `bench/cold.go` 35, `bench/hot.go` 15, `bench/command.go` 20 |

`P` is `cmd/stellar-rpc/internal/rpcv2`. All identifiers below were read at `91f158b`.

## 1. Goal

After this PR, `bench-ingest cold` and `bench-ingest hot` leave a catalog at `<dataset>/catalog/rocksdb`. The catalog pins the earliest ledger. A cold dataset also holds a ready, empty hot chunk database for the frontier chunk (last chunk + 1). A run into a dataset that already has a catalog fails before it writes a file. `query.NewReadView` succeeds on both kinds of dataset.

## 2. Scope

- Replace `openScratchCatalog` (`P/bench/scratch.go`) with `checkNoCatalog` and `createDatasetCatalog` in a new file `P/bench/dataset.go`. Delete `scratch.go`.
- Call them from `runCold` (`P/bench/cold.go`) and `runHot` (`P/bench/hot.go`). Pin `config:earliest_ledger`.
- Add `createFrontierChunk` to `runCold`: the hot create sequence for chunk end + 1.
- Remove the `--catalog-dir` flag and the `CatalogDir` option fields (`P/bench/command.go`, `cold.go`, `hot.go`).
- Tests in `P/bench/bench_test.go`.

## 3. Out of scope (boundaries)

- `results.json`, `--paced-ledgers`: PR 03.
- The read-only opens (`catalog.OpenReadOnly`, `hotchunk.OpenReadOnlyWithEvents`): PR 01. This PR's tests open the kept catalog with `catalog.Open`. If PR 01 has merged, use `catalog.OpenReadOnly` in the tests.
- `bench-serve` and its start sequence: PR 05.
- `bench-ingest freeze`: PR 12.
- A `dataset.json` manifest: rejected by D20. The catalog holds no passphrase, range or tier.
- A frontier key without a database: rejected. A `ready` key without a directory breaks the ready-open rule (`hotchunk.OpenReadyView` fails) and `refineWithHotDB` in `P/progress.go` (review digest R1 verify note).

## 4. Design

### 4.1 Current code

- `openScratchCatalog(catalogBase, layout, logger)` makes `<base>/bench-ingest-catalog-XXXX/catalog` with `os.MkdirTemp` and returns a release func that closes the catalog and removes the directory. `catalogBase` is `--catalog-dir`, else `--cold-out-dir` or `--hot-dir`.
- `runCold`: `validate`, `os.MkdirAll(opts.OutDir)`, `config.PrepareRoots(layout.LedgersRoot(), layout.EventsRoot(), layout.EventsIndexRoot(), layout.TxHashRawRoot(), layout.TxHashIndexRoot())`, `openScratchCatalog`, `openSource`, `backfill.RunBackfill`, CSV.
- `runHot`: `validate`, `os.MkdirAll(opts.OutDir)`, `config.PrepareRoots(layout.HotRoot())`, `openScratchCatalog`, `openSource`, `rpcv2.RunBoundedIngestionLoop`, CSV.
- Nothing pins `config:earliest_ledger`; only the daemon's `validateConfig` does (facts A0).
- `geometry.NewLayout(root).CatalogPath()` is `<root>/catalog/rocksdb`.

### 4.2 New functions (`P/bench/dataset.go`)

```go
var errDatasetExists = errors.New("dataset already has a catalog")
func checkNoCatalog(layout geometry.Layout) error
func createDatasetCatalog(layout geometry.Layout, start chunk.ID, logger *supportlog.Entry) (*catalog.Catalog, error)
func createFrontierChunk(cat *catalog.Catalog, c chunk.ID, logger *supportlog.Entry) error
```

- `checkNoCatalog`: `os.Stat(layout.CatalogPath())`. No error: return `fmt.Errorf("%w: %s (use a new dataset root)", errDatasetExists, path)`. `fs.ErrNotExist`: return nil. Any other error: return it.
- `createDatasetCatalog`: `geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)`, then `catalog.Open(layout.CatalogPath(), layout, txLayout, logger)`, then `cat.PinEarliestLedger(start.FirstLedger())`. On a pin error, close the catalog and return the error. The caller closes the catalog with one `defer cat.Close()`. The catalog is never removed.
- `createFrontierChunk`: the sequence of `openHotDBForChunk` in `P/hotloop.go` (unexported in package `rpcv2`, so bench cannot call it):
  1. `cat.BeginHotCreate(c)` (wipes a leftover directory, writes `transient`);
  2. `db, err := hotchunk.Open(cat.Layout().HotChunkPath(c), c, logger)`;
  3. `cat.FinishHotCreate(c)` (fsyncs the directory and its parent, writes `ready`);
  4. `db.Close()`. Return a close error.

  On an error after step 2, close `db` once and return the first error. This is the smallest correct way: it uses the same catalog bracket as the ingestion loop, and the empty database opens with every ready-open function. An empty hot database is a normal state: warmup builds empty mirrors (`warmup` doc in `P/stores/event/hot_store.go`), and `refineWithHotDB` returns `chunk.LastLedgerOf(live - 1)` for it.

### 4.3 `runCold` order

1. `opts.validate()`. Add: end + 1 must be at or below `maxChunkID`, because the frontier chunk needs a valid `LastLedger`.
2. `checkNoCatalog(layout)`. This runs before any write: before `--out`, before the roots, before the catalog.
3. `os.MkdirAll(opts.OutDir)`.
4. `config.PrepareRoots(...)`, now with `layout.HotRoot()` added (the frontier chunk lives there).
5. `createDatasetCatalog(layout, opts.StartChunk, logger)`; `defer cat.Close()`.
6. `openSource`, then the timed `backfill.RunBackfill` (no change).
7. On success only: `createFrontierChunk(cat, end+1, logger)`. It runs after `totalWall`, so it is not in `backfill_wall` or the total wall log line. On a backfill error, skip it: the dataset is incomplete anyway.
8. CSV output (no change).

### 4.4 `runHot` order

1. `opts.validate()`.
2. `checkNoCatalog(layout)`.
3. `os.MkdirAll(opts.OutDir)`, `config.PrepareRoots(layout.HotRoot())`.
4. `createDatasetCatalog(layout, opts.StartChunk, logger)`; `defer cat.Close()`.
5. The loop, unchanged. `BeginHotCreate` and `FinishHotCreate` already run inside `rpcv2.RunBoundedIngestionLoop` for each chunk, so each ingested chunk is `ready` in the kept catalog (facts A2).

### 4.5 Catalog after each run

| Key | Cold run, chunks `[s, e]` | Hot run, chunks `[s, e]` |
|---|---|---|
| `chunk:{c}:{ledgers,events,txhash}` | `frozen` for `s..e` | absent |
| `index:{idx}:{lo}:{hi}` | `frozen` coverage (partial window is capped at `e`, facts A2) | absent |
| `hot:chunk:{c}` | `ready` for `e+1` only (empty database at `<root>/hot/<e+1>`) | `ready` for each chunk the loop opened |
| `config:earliest_ledger` | `s × 10000 + 2` | `s × 10000 + 2` |
| `meta/catalog-secret` | minted by `catalog.Open` | minted by `catalog.Open` |

For a cold dataset, `LastCompleteChunk` is `e` (highest ready minus one), and `lastCommittedLedger` gives `e.LastLedger()`: the highest durable chunk is `e`, and the empty frontier refines to `LastLedgerOf(e)`.

### 4.6 `--catalog-dir`

Remove the flag from `newColdCommand` and `newHotCommand`, and the `CatalogDir` field from `coldOptions` and `hotOptions`. Reason: `bench-serve` and the daemon build the layout from one root (`geometry.NewLayout`), so a catalog elsewhere makes the dataset unreadable. No test and no script in stellar-rpc sets it. The benchmarks runner (`runner/internal/plan/plan.go`) does not pass it. Rewrite the help texts of `--cold-out-dir` and `--hot-dir`: "dataset root (required; must not hold a catalog)". Rewrite the `ColdRoot` and `HotRoot` field docs, which today say "scratch" and "a re-run over the same range overwrites freely".

### 4.7 Separate roots (D14)

The runner uses `<root>/<profile>/run<N>/cold` and `.../hot`. A hot run into a cold root, or a cold run into a hot root, fails at `checkNoCatalog`. No other check is needed.

Example:

```
stellar-rpc-v2 bench-ingest cold --source pack --pack-dir /mnt/nvme/bench/sac-6000/packs \
  --start-chunk 1 --num-chunks 2 --workers 8 --cold-out-dir /mnt/nvme/bench/sac-6000/run1/cold --out <step dir>
# leaves .../run1/cold/catalog/rocksdb and .../run1/cold/hot/00000003
```

## 5. Files

| File | Change | What | Non-test lines |
|---|---|---|---|
| `P/bench/scratch.go` | delete | `openScratchCatalog`, `catalogBaseDirPerm` | 42 removed |
| `P/bench/dataset.go` | new | `errDatasetExists`, `checkNoCatalog`, `createDatasetCatalog`, `createFrontierChunk` | 60 |
| `P/bench/cold.go` | modify | order of 4.3, frontier call, `validate` bound, docs, drop `CatalogDir` | 35 |
| `P/bench/hot.go` | modify | order of 4.4, docs, drop `CatalogDir` | 15 |
| `P/bench/command.go` | modify | drop `--catalog-dir`, help texts | 20 |
| `P/bench/bench_test.go` | modify | new and changed tests | test |

The count of 130 includes the 42 lines removed from `scratch.go`.

## 6. Tests

All tests use the existing helpers in `P/bench/bench_test.go`: `writeSourcePack`, `testLogger`, `readCSV`. Add one helper `openKeptCatalog(t, root) *catalog.Catalog` (opens `geometry.NewLayout(root).CatalogPath()`, closes on cleanup) and one helper `readViewOn(t, cat, latest uint32)` (below).

`readViewOn` does what `bench-serve` will do (PR 05), with the calls the facts name (A2, A3, A4):

1. `reg := query.NewRegistry(cat, rpcv2test.RetentionFor(t, cat, 0))`. `RetentionFor` reads `config:earliest_ledger`, so a missing pin gives chunk 0 and `SeedCloseTimes` fails ("oldest ledger ... missing").
2. For each `c` in `cat.ReadyHotChunkKeys()`: `db, _ := hotchunk.OpenReadyView(geometry.HotReady, cat.Layout().HotChunkPath(c), c, testLogger())`; `reg.PublishHandle(c, db)`; close on cleanup. (With PR 01 merged, use `hotchunk.OpenReadOnlyWithEvents`.)
3. `reg.SetLatestLedger(latest, query.UnknownCloseTime())`. Without it the latest ledger is 0 and `SeedCloseTimes` does nothing.
4. `view, err := reg.NewReadView()`; `require.NoError`; `view.Release()`.
5. `require.NoError(t, adapters.SeedCloseTimes(reg))`. This reads the oldest and the latest ledger, so it proves the retention window and the latest ledger are right.

| Test | Package/file | Proves | How |
|---|---|---|---|
| `TestRunColdKeepsCatalog` | `bench`, `bench_test.go` | A cold run leaves a catalog with the pin, frozen chunks and a ready frontier; `NewReadView` succeeds. | `writeSourcePack` for chunk 0 (10,000 ledgers); `runCold`; `openKeptCatalog`; assert `EarliestLedger() == chunk.ID(0).FirstLedger()`, `State(0, KindLedgers) == frozen`, `HotState(1) == ready`, `DirExists(layout.HotChunkPath(1))`; `readViewOn(t, cat, chunk.ID(0).LastLedger())`. |
| `TestRunColdMultiChunk` (existing, extend) | same | The frontier is end + 1 for two chunks. | Add `HotState(2) == ready` and `ReadyHotChunkKeys() == [2]`. |
| `TestRunHotKeepsCatalog` | same | A hot run leaves a catalog with the pin and ready chunks; `NewReadView` succeeds. | `writeSourcePack` 200 ledgers; `runHot` with `NumLedgers: 200`; `HotState(0) == ready`; `readViewOn(t, cat, first+199)`. |
| `TestRunColdRefusesExistingDataset` | same | A second run fails with `errDatasetExists` and writes no file. | Run once. Take the file set of the root (with `fileset.Take` from PR 01 if merged, else a 15-line `filepath.WalkDir` helper in the test file). Run again with a new `OutDir`. `errors.Is(err, errDatasetExists)`; file set unchanged; `NoDirExists(newOut)`. |
| `TestRunHotRefusesExistingDataset` | same | Same for hot. Replaces the last line of `TestRunHotFromPack`, which today asserts that a second run succeeds. | Same pattern. |
| `TestRunHotRefusesColdDataset` | same | D14: a hot run into a cold root fails. | `runCold` then `runHot` on the same root; `errDatasetExists`. |
| `TestBenchRejectsInvalidSourceEarly` (existing) | same | Validation still runs before any directory is created. | No change. |

Test time: the cold tests use one or two full chunks, as the existing cold tests do. The hot tests use 200 ledgers.

## 7. Done when

| Spec item | Check |
|---|---|
| A cold run and a hot run each leave a catalog. | `TestRunColdKeepsCatalog`, `TestRunHotKeepsCatalog` pass. |
| `query.NewReadView` succeeds on both catalogs. | Both tests call `readViewOn`, which asserts `NewReadView` and `adapters.SeedCloseTimes` return no error. |
| A second run into the same root fails and writes no file. | `TestRunColdRefusesExistingDataset`, `TestRunHotRefusesExistingDataset` pass. |
| Pin is set. | Both keep-catalog tests assert `EarliestLedger`. |
| No scratch catalog remains. | `grep -rn 'openScratchCatalog\|bench-ingest-catalog-\|catalog-dir' cmd/stellar-rpc/internal/rpcv2/bench` returns nothing. |

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
go test -race ./cmd/stellar-rpc/internal/rpcv2/catalog/... ./cmd/stellar-rpc/internal/rpcv2/query/...
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'
```

Manual check: build the v2 binary, run `bench-ingest cold` over a small pack tree twice into one root. The second run exits non-zero with "dataset already has a catalog" and `ls -lR` of the root is unchanged.

## 9. Risks and open points

- The benchmarks runner on `main` (`runner/internal/plan/plan.go`) runs `bench-ingest cold` once per chunk into one `--cold-out-dir` (`root + ".partial"`) for its golden datasets. With this PR the second chunk fails at the existence check. Its ingest steps clean the directory first (`PreClean`), so they still work. Options: (a) accept it, because B4 removes that runner; (b) tell the benchmarks owners before the merge. Record the choice in the decision log. Until PR 07, campaigns with multi-chunk golden datasets on the old runner break.
- A failed run leaves a partial dataset with a catalog. A rerun into the same root fails. This is intended; the error text says to use a new root.
- The frontier creation takes a few milliseconds (an empty RocksDB open and close, plus two fsyncs). It is outside `backfill_wall` and outside the total wall log line.
- `TestRunHotFromPack` asserts today that a second run into the same root succeeds. This PR inverts that behaviour. Move the assertion into `TestRunHotRefusesExistingDataset`.
- The cold dataset now has a `hot/` directory. PR 05 must open it (it is `ready`) and PR 05's file-set test covers it.
- The review digest (R1) placed this work in "PR 3"; the spec now numbers it PR 02. No other change.
- No new decision is needed if the design above holds. If the reviewer keeps `--catalog-dir`, record it as a new decision with the reason.
