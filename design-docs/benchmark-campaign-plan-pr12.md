# PR 12: `bench-ingest freeze`

| | |
|---|---|
| Branch | `bench-campaign-v2/12-freeze` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 02 (kept catalog, pinned earliest ledger), PR 03 (`results.json` writer). D23, D33 (the runner runs the freeze after the hot load step) and D37 (the range, the second-freeze error), all confirmed. |
| Implements | D23, D37; spec Section 6.8, Section 7.2 rule 9 (`freeze` keys); requirement R1 "freezing" (requirements evaluation) |
| Estimate | About 170 non-test lines: `bench/freeze.go` 100, `bench/cold.go` 35 (shared driver), `bench/command.go` 35 |

`P` is `cmd/stellar-rpc/internal/rpcv2`. All identifiers below were read at `91f158b`.

## 1. Goal

After this PR, `bench-ingest freeze --dataset <hot dataset>` times the lifecycle freeze of the complete ready hot chunks of a kept hot dataset. The source of each chunk is its hot chunk database, as in the daemon. The command writes `results.json` with `command: freeze` and the same measurement names as `bench-ingest cold`. After it, the read path resolves the frozen chunks to cold.

Requirement R1 context (requirements evaluation, section R1 (d) and (e)):

- The lifecycle freeze is `backfill.RunBackfill` over `[floor, lastChunk]` (`runLifecycle` in `P/lifecycle/lifecycle.go`, stage 1).
- The only difference from `bench-ingest cold` is the ledger source. `backfillSource` (`P/backfill/process.go`) takes a ready, complete hot database first (`resolveHotSource`, `tryHotSource`, opened with `hotchunk.OpenReadyView`).
- `bench-ingest cold` therefore measures a freeze from packs. Nothing times a freeze from a hot database today. `e2e_test.go` runs the real freeze but only counts it. `backfill/hotsource_test.go` tests source selection only. Production records `phase_duration_seconds{phase="freeze"}`, the only freeze timer.
- Discard and prune (lifecycle stages 2 and 3) stay untimed (spec Section 1.1).

## 2. Scope

- New `P/bench/freeze.go`: `freezeOptions`, `runFreeze`, `completeHotRange`.
- `P/bench/cold.go`: move the timed `RunBackfill` body of `runCold` into one driver function that `runCold` and `runFreeze` share.
- `P/bench/command.go`: `newFreezeCommand`, registered in `NewCommand`; `newBenchCommand` accepts a nil `*sourceFlags`.
- Tests in `P/bench/freeze_test.go`.

## 3. Out of scope (boundaries)

- The discard of hot chunks after the freeze: not done (spec 6.8 "It does not discard the hot chunks").
- The freeze step in the runner and its place after `load-hot`: PR 07 (D33). The runner marks the step `skipped` when the binary has no `bench-ingest freeze`.
- Timing through the registry's shared handle (`ProcessConfig.HotHandle`), as the live daemon does: not possible without a live writer. `bench-live` (PR 13) covers the live freeze through `phase_duration_seconds{phase="freeze"}`.
- A BSB or pack fallback source: none. A chunk that is not complete in the hot tier is not frozen (Section 4.3).

## 4. Design

### 4.1 Command

```
stellar-rpc-v2 bench-ingest freeze --dataset /mnt/nvme/bench/sac-6000/run1/hot \
  --workers 8 --out <step dir>
```

Flags: `--dataset` (required), `--workers` (default 1, as `cold`), `--out`, `--cpuprofile`, `--memprofile`. No source flags. `newBenchCommand(use, short string, src *sourceFlags, prof *profileFlags, run ...)` binds `src` today; change it to skip `src.bind` when `src == nil`.

### 4.2 Order in `runFreeze`

```go
type freezeOptions struct { Dataset string; Workers int; OutDir string }
func runFreeze(ctx context.Context, logger *supportlog.Entry, opts freezeOptions) (err error)
```

1. `opts.validate()`: `Dataset != ""`, `Workers >= 1`.
2. `layout := geometry.NewLayout(opts.Dataset)`. `os.Stat(layout.CatalogPath())` must succeed. Reason: `catalog.Open` creates a missing catalog (facts A2), so a wrong `--dataset` would freeze nothing into a new empty catalog.
3. `cat, err := catalog.Open(layout.CatalogPath(), layout, txLayout, logger)`; `defer cat.Close()`. Read-write: the freeze writes cold files and catalog entries (spec 6.8). The kept catalog holds the catalog secret that PR 02 minted; `WriteColdChunk` reads it through `ingestConfigFor`.
4. `lo, hi, err := completeHotRange(cat, logger)` (4.3).
5. Refuse a frozen dataset: when `cat.State(c, geometry.KindLedgers)` is not empty for any `c` in `[lo, hi]`, return `errAlreadyFrozen`. D37: a second freeze of a dataset returns an error. Without the check it would resolve an empty plan and time nothing.
6. `config.PrepareRoots(layout.LedgersRoot(), layout.EventsRoot(), layout.EventsIndexRoot(), layout.TxHashRawRoot(), layout.TxHashIndexRoot())`, the same call as `runCold`.
7. `os.MkdirAll(opts.OutDir)`; results `start` with `command: "freeze"` (PR 03 writer).
8. The shared driver (4.4) with `Backend: nil` and `lo`, `hi`.
9. Results `finish` and CSV, as `runCold` does.

### 4.3 Range: `completeHotRange`

```go
func completeHotRange(cat *catalog.Catalog, logger *supportlog.Entry) (lo, hi chunk.ID, err error)
```

- `ready, _ := cat.ReadyHotChunkKeys()` (sorted ascending). Fail with "no ready hot chunk" when empty.
- For each `c` in order: `db, _ := hotchunk.OpenReadyView(geometry.HotReady, layout.HotChunkPath(c), c, logger)`; `seq, ok, _ := db.MaxCommittedSeq()`; `db.Close()`. Complete means `ok && seq >= c.LastLedger()`, the rule of `tryHotSource`.
- The range is the longest run of consecutive complete chunks from `ready[0]`. Stop at the first incomplete chunk or gap. Fail when the first chunk is incomplete.
- These opens run before the timed region.

Rule (D37): all consecutive complete ready hot chunks from the lowest, including the top chunk when it is complete. Reason: the lifecycle freezes `[floor, lastChunk]`, where `lastChunk` is `LastCompleteChunk` (highest ready minus one), because the highest ready chunk is the live chunk. A bench hot dataset has no live chunk: its top chunk can be complete (`--num-chunks 2`, no `--num-ledgers`). An incomplete chunk in the range would fall through `backfillSource` to "no local copy and no bulk backend" and fail the run.

### 4.4 Shared driver (`cold.go`)

Move the body between `sink := newCSVSink()` and the CSV write of `runCold` into:

```go
type backfillRun struct {
    Catalog *catalog.Catalog; Backend backfill.Backend; Start, End chunk.ID; Workers int; OutDir string
}
func runMeasuredBackfill(ctx context.Context, logger *supportlog.Entry, r backfillRun, sink *csvSink) (time.Duration, error)
```

It calls `backfill.RunBackfill(ctx, backfill.ExecConfig{Catalog, Logger, Metrics: sink, Process: backfill.ProcessConfig{Sink: sink, Backend: r.Backend}, Workers, MaxRetries: 0}, r.Start, r.End)`, `recordPeakRSS`, `writePartialCSVs` on error, `logSummary`, `logColdWall`. Both commands use it, so the rows are the same: `driver.backfill_wall`, `driver.index_rebuild`, `driver.chunk_total`, `driver.{ledgers,txhash,events}_total`, `driver.cold_extract`, `driver.peak_rss`, and the `ledgers`, `txhash`, `events` stage rows (D34 names, PR 03).

With `Backend: nil` and `HotHandle: nil`, `backfillSource` takes branch (1) for each chunk: `resolveHotSource` reads the `ready` key and `tryHotSource` opens the database with `hotchunk.OpenReadyView` and streams `db.Source()`. The source log line is `"source": "hot tier"`.

### 4.5 Parameters in `results.json`

`startChunk` (`lo`), `numChunks` (`hi - lo + 1`), `workers`, `source: "hot"`, `txhashIndexChunks` (same computation as PR 03 for cold), `readyHotChunks` (the count of ready keys, so a reader sees chunks left out by 4.3), and `flags`, the raw flag map (spec 7.2 rule 9).

### 4.6 Catalog before and after

Hot dataset from PR 02 with `--start-chunk s --num-chunks 2`, both chunks complete:

| Key | Before | After |
|---|---|---|
| `hot:chunk:{s}`, `hot:chunk:{s+1}` | `ready` | `ready` (unchanged; no discard) |
| `chunk:{c}:{ledgers,events,txhash}` | absent | `frozen` for `s`, `s+1` |
| `index:{idx}:{lo}:{hi}` | absent | `frozen` coverage `[s, s+1]` (partial window, capped at range end, facts A2) |
| `config:earliest_ledger` | `s × 10000 + 2` | unchanged |
| `meta/catalog-secret` | from PR 02 | unchanged |

Files added: `ledgers/`, `events/data/`, `events/index/`, `txhash/raw/` (`.bin` files stay for a partial window, facts A0), `txhash/index/`. The files under `hot/` do not change: every open of a hot database is read-only.

Read path after the freeze: `ReadView.resolveTier` picks cold when the chunk's key is `frozen` ("frozen cold wins", facts A1), for each kind. `NewReadView` still succeeds, because the `ready` hot keys remain. The runner runs the freeze after the hot load step (D33), so no load step sees this state.

## 5. Files

| File | Change | What | Non-test lines |
|---|---|---|---|
| `P/bench/freeze.go` | new | `freezeOptions`, `validate`, `runFreeze`, `completeHotRange`, `errAlreadyFrozen`, `parameters()` | 100 |
| `P/bench/cold.go` | modify | `backfillRun`, `runMeasuredBackfill`; `runCold` calls it | 35 |
| `P/bench/command.go` | modify | `newFreezeCommand`, `NewCommand` adds it, nil `src` in `newBenchCommand`, doc of `NewCommand` | 35 |
| `P/bench/README.md` | modify | `command: freeze`, its parameters | not counted |
| `P/bench/freeze_test.go` | new | tests | test |

## 6. Tests

Test dataset: `runHot` over a pack tree from `writeSourcePack` (`bench_test.go`). One complete chunk (10,000 ledgers of chunk 0) plus 50 ledgers of chunk 1 (`NumChunks: 2`, `NumLedgers: 10050`). Chunk 1 is ready and incomplete, so the dataset also tests the range rule. Build it once per test with a helper `keptHotDataset(t) (root string)`. `writeSourcePack` puts one transaction with one event in every 100th ledger, so the freeze writes real events and tx-hash files.

| Test | Package/file | Proves | How |
|---|---|---|---|
| `TestRunFreezeWritesResults` | `bench`, `freeze_test.go` | Spec: a freeze over a kept hot test dataset writes `results.json`. | `runFreeze(ctx, testLogger(), freezeOptions{Dataset: root, Workers: 2, OutDir: out})`; `readResults` (PR 03 helper): `command == "freeze"`, `status == "ok"`, `parameters.startChunk == 0`, `numChunks == 1`, `readyHotChunks == 2`, `driver.backfill_wall.count == 1`, `driver.ledgers_total.items == 10000`. |
| `TestRunFreezeResolvesCold` | same | Spec: after the freeze, `query.NewReadView` resolves the frozen chunks to cold. | Open the catalog; `query.NewRegistry(cat, rpcv2test.RetentionFor(t, cat, 0))`; publish no handle; `SetLatestLedger(chunk.ID(0).LastLedger(), query.UnknownCloseTime())`; `view.WithLedger(seq, fn)` succeeds for the first and last ledger of chunk 0. With no hot handle, a hot-routed read returns `ErrUnavailable`, so success proves the cold route. `cat.State(0, KindEvents) == frozen`. |
| `TestRunFreezeLeavesHotUnchanged` | same | The freeze does not change the hot databases. Walk `layout.HotRoot()` with `filepath.WalkDir` before and after, inside the test, and compare the relative path, size and modification time of each file. |
| `TestRunFreezeRefusesSecondRun` | same | A second freeze returns `errAlreadyFrozen` and times nothing. | Run twice. |
| `TestRunFreezeRequiresCatalog` | same | A missing catalog fails and creates nothing. | `Dataset: t.TempDir()`; error; `NoDirExists(layout.CatalogPath())`. |
| `TestCompleteHotRange` | same | The range stops at the first incomplete chunk and fails when the first is incomplete. | Case 1: `keptHotDataset` gives `[0, 0]`. Case 2: `runHot` with 50 ledgers of chunk 0 gives an error. Do not use `rpcv2test.SeedHotChunkSeq` here: it keeps its read-write handle open until cleanup, and a read-only open beside a writer is undefined behaviour. |
| `TestRunColdFromPack` (existing) | `bench_test.go` | The shared driver keeps cold output. | No change. |
| `TestNewCommand` (existing, extend) | `command_test.go` | `freeze` is registered; `--dataset` is required; no `--source` flag. | Look up the subcommand and flags. |

Test time: 10,050 fsync'd hot commits for the dataset. Measure it under `-race`. If one test binary takes too long, build the dataset once in `TestMain` or share it through a `sync.Once` helper.

## 7. Done when

| Spec item | Check |
|---|---|
| A freeze over a kept hot test dataset writes `results.json`. | `go test -race -run TestRunFreezeWritesResults ./cmd/stellar-rpc/internal/rpcv2/bench/` |
| After it, `query.NewReadView` resolves the frozen chunks to cold. | `go test -race -run TestRunFreezeResolvesCold ./cmd/stellar-rpc/internal/rpcv2/bench/` |
| Same measurement names as `cold` (spec 6.8). | `TestRunFreezeWritesResults` asserts the `driver.*` names that `TestRunColdWritesResults` (PR 03) asserts. |
| Hot chunks kept (spec 6.8). | `TestRunFreezeLeavesHotUnchanged`; `cat.HotState` stays `ready`. |

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
go test -race ./cmd/stellar-rpc/internal/rpcv2/backfill/...
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'
```

Manual check: build with `make build-rpc-v2`. Run `bench-ingest hot --num-chunks 2 --close-interval 0` on a two-chunk pack tree, then `bench-ingest freeze --dataset <root> --workers 4 --out /tmp/fz`. Check `jq .status,.parameters /tmp/fz/results.json` and the log line `"source": "hot tier"` for each chunk.

## 9. Risks and open points

- The daemon freezes through the registry's shared read-write handle (`ProcessConfig.HotHandle`), whose block cache holds recent data. The bench opens each database read-only with an empty block cache. The OS page cache state depends on what ran before (D33: the hot load step). Record the step order in `campaign.json` (PR 07); the README states the difference.
- The range rule (4.3, D37) freezes the top chunk when it is complete. The lifecycle never freezes the live chunk. For the campaign's two full chunks this freezes both, which is what the step name says. A change to the rule needs an amendment of D37 first.
- `errAlreadyFrozen` makes a rerun fail. The runner must use a fresh hot dataset for each freeze step; it does (each run has its own datasets, spec Section 4).
- `completeHotRange` opens each ready chunk read-only once before the timed region. With the default 512 MB block cache per database (facts), two sequential opens allocate and free the cache; no effect on the timing.
- The estimate in the requirements evaluation (60 to 100 lines) did not include the shared-driver refactor and the command wiring. 170 lines is still well under 600.
- Verified: `ExecConfig.processConfig` (`P/backfill/execute.go`) copies `Catalog` and `Logger` into `ProcessConfig`, so the driver passes only `Sink` and `Backend`, as `runCold` does. `HotHandle` stays nil.
