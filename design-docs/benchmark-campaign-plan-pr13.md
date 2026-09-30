# PR 13: bench-live

| | |
|---|---|
| Branch | `bench-campaign-v2/13-bench-live` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 05 (the shared `--profile-rates` flag in `P/bench/profile.go`). D25 (confirmed; amended on 2026-09-30: the three `rpcv2.Options` fields, no `ServeReads` seam). |
| Implements | D24 (flag on `bench-live`), D25; spec Sections 6.9 and 6.10; spec Section 10.1 row 13; requirement R7 (requirements-eval.md) |
| Estimate | About 250 non-test lines: `P/daemon.go` 20, `P/bench/live.go` 160, `P/bench/live_source.go` 70, `cmd/stellar-rpc/rpcv2/main.go` 1 |

## 1. Goal

After this PR, `stellar-rpc-v2 bench-live` runs the real daemon body (`startup.go` `run`) over a local ledger pack tree.
It ingests the ledgers at the close interval, runs the lifecycle, and serves JSON-RPC on the same process.
So a client (Blaster, later) can load a node while it ingests.
PR 13 delivers the command and a local test only. Campaign use needs a new PR after PR 10 and PR 13 (D25).

## 2. Scope

- Add three fields to the exported `rpcv2.Options` (`Core`, `Backend`, `NetworkPassphrase`) and pass them to `daemonOptions` (`P/daemon.go`). Make `resolveCore` keep the passphrase for an injected core.
- Add the paced live source over the bench pack source: a `CoreOpener`, its stream, and a `backfill.Backend` with a fixed tip (new file `P/bench/live_source.go`).
- Add the command `bench-live`: flags, the config file it writes, and the call to `rpcv2.RunDaemonWithOptions` (new file `P/bench/live.go`).
- Register the command in `cmd/stellar-rpc/rpcv2/main.go`.
- Add the local test of Section 6 (new file `P/bench/live_test.go`).

## 3. Out of scope (boundaries)

- Campaign use (a runner step, `load.json` fields, Blaster flags): a new PR after PR 10 and PR 13 (D25, spec 1.1).
- Ingestion inside `bench-serve`: rejected by D25. `bench-serve` stays read-only (D5).
- `bench-ingest hot` and `bench-serve` on one dataset at the same time: rejected by D25 (RocksDB read-only beside a writer is undefined).
- A BSB or captive-core source for `bench-live`: not needed now (requirements-eval R4/R5).
- Storage metrics: PR 04. `bench-live` exposes them when PR 04 is merged, with no extra code.

## 4. Design

### 4.1 Current code (verified at 91f158b)

- `run(ctx, cfg StartConfig)` (`P/startup.go`): `requirePinnedEarliest`, `lastCommittedLedger`, `backfillToTip`, `openHotDBForChunk`, `cfg.Core.OpenCore(ctx)`, `query.OpenRegistry`, `adapters.SeedCloseTimes`, `replayFeeWindows`, the read listener on `cfg.Endpoint`, then an errgroup of `runIngestionLoop`, `lifecycle.Loop` and `cfg.ServeReads`.
- `runDaemonWith(ctx, configPath, daemonOptions)` (`P/daemon.go`) builds `run`'s `StartConfig`. `daemonOptions` has `Backend backfill.Backend`, `Core CoreOpener`, `ServeReads`, `Logger`, `Metrics`, `IngestSink`, `Flags`, `OnListen` and two test-only fields. It is unexported. `rpcv2/bench` imports `rpcv2`, so `bench` can reach only exported names.
- The exported path is `RunDaemonWithOptions(ctx, configPath, Options)`. `Options` has `Logger`, `Flags` and `OnListen func(rpc, admin net.Addr)`. It maps them into `daemonOptions`.
- `CoreOpener` is exported (`P/startup.go`): `OpenCore(ctx) (ledgerbackend.LedgerStream, error)`. `backfill.Backend` is exported: `ledgerbackend.LedgerStream` plus `Tip(ctx) (uint32, error)`.
- `resolveCore(opts, cfg, logger)` returns `resolvedCore{live: opts.Core, backfill: opts.Core}` for an injected core. So `networkPassphrase` is empty. That value feeds `newPreflightPool`, `handlerParams.networkPassphrase` and so `adapters.NewTransactionReader`. With an empty passphrase `getTransaction` cannot verify a hash (D20).
- `corestate.New` and `newPreflightPool` need no running core. `e2e_test.go` `runDaemonInBackground` and `metrics_test.go` `TestRunDaemon_ExposesProcessMetrics` run `runDaemonWith` with an injected `Core` and `Backend` and pass. So corestate and the preflight pool are safe with no core.
- `runIngestionLoop` asks for `ledgerbackend.UnboundedRange(cfg.Resume)` (`P/hotloop.go`). A stream that ends returns `errStreamEnded`, and the daemon exits with an error.
- Bench pack source: `openSource(ctx, sourceConfig)` returns `packBackend{root}` (`P/bench/sources.go`). `packBackend.Tip` returns `math.MaxUint32`. The pack stream refuses a range outside its coverage (`TestRunHotIncompleteStream`).
- Bench pace code (`P/bench/pace.go`): `newPaceSchedule(interval, firstSeq)`, `pacingStream{inner, schedule, sleep}`, `contextSleep`. `buildHotStream` (`P/bench/hot.go`) composes `boundedStream` (ignores the requested range) and `pacingStream`.
- First start with a numeric `earliest_ledger` needs `floor <= tip` (`resolveEarliestFirstStart`, `P/config_validate.go`). `passTarget(tip, lastCommitted)` gives `chunk.LastCompleteChunkAt(max(tip, lastCommitted))` (`P/startup.go`).

### 4.2 Exported seams (`P/daemon.go`)

Add to `Options`:

```go
Core              CoreOpener       // nil ⇒ captive core from [ingestion]
Backend           backfill.Backend // nil ⇒ built from [backfill.datastore] or the archives
NetworkPassphrase string           // used only with Core; empty ⇒ unknown
```

- `RunDaemonWithOptions` copies them into `daemonOptions`. Add `networkPassphrase string` to `daemonOptions`.
- `resolveCore`: for `opts.Core != nil`, return `resolvedCore{live: opts.Core, backfill: opts.Core, networkPassphrase: opts.networkPassphrase}`.
- `ServeReads` stays nil: `bench-live` serves with the production `newServeReads`. D25 (amended): `bench-live` does not need the `ServeReads` seam, so `Options` gets no `ServeReads` field.

### 4.3 Live source (`P/bench/live_source.go`)

- `type liveCore struct{ src ledgerbackend.LedgerStream; last uint32; interval time.Duration }`. `OpenCore` returns a `liveStream`.
- `liveStream.RawLedgers(ctx, rng, opts...)`: serve `BoundedRange(rng.From(), last)` from `src`. When `interval > 0`, pass it through `pacingStream` with `newPaceSchedule(interval, rng.From())`. After `last`, block on `<-ctx.Done()` and yield `ctx.Err()`. So the node holds a static head and keeps serving until SIGTERM, and the loop does not exit on `errStreamEnded`.
- `type liveBackend struct{ ledgerbackend.LedgerStream; tip uint32 }`. `Tip` returns the first ledger of `--start-chunk`. With `earliest_ledger` = that ledger, `resolveEarliestFirstStart` passes (`floor == tip`) and `passTarget` gives chunk `start-1`, below the floor, so `backfillToTip` has nothing to do. Ingestion starts at the earliest ledger.

### 4.4 Command (`P/bench/live.go`)

```
stellar-rpc-v2 bench-live --pack-dir /mnt/nvme/bench/sac-6000/packs --start-chunk 1 \
  --num-ledgers 20000 --close-interval 600ms --data-dir /mnt/nvme/bench/live \
  --listen 127.0.0.1:8000 --admin-listen 127.0.0.1:8001 \
  --network-passphrase "<passphrase of the packs>" --profile-rates 0,0
```

- `func NewLiveCommand() *cobra.Command`. `main.go`: `rootCmd.AddCommand(bench.NewLiveCommand())`.
- Flags: `--pack-dir` (required), `--start-chunk` (required), `--num-ledgers` (0 = to the end of `--start-chunk`), `--close-interval` (0 = back to back), `--data-dir` (required), `--listen`, `--admin-listen`, `--network-passphrase` (required, non-empty), `--profile-rates` (PR 05 `rateFlags`).
- Validation, one time at the boundary: empty passphrase fails; a catalog at `<data-dir>/catalog/rocksdb` fails (decide in PR: refuse, as D4 does for `bench-ingest`, or allow a restart; this plan refuses, for a known start state).
- `runLive(ctx, logger, liveOptions) error` writes `<data-dir>/bench-live.toml`, then calls `rpcv2.RunDaemonWithOptions(ctx, path, rpcv2.Options{Logger, OnListen, Core: liveCore, Backend: liveBackend, NetworkPassphrase})`. `OnListen` writes one line `ready` to stdout.

Config file it writes:

```toml
[storage]
default_data_dir = "<data-dir>"
[retention]
earliest_ledger = "<first ledger of --start-chunk>"
retention_chunks = 0
[service]
endpoint = "<--listen>"
admin_endpoint = "<--admin-listen>"
[service.methods.getHealth]
max_healthy_ledger_latency = "1000000h"
[logging]
level = "info"
```

- `max_healthy_ledger_latency` is high because synthetic packs carry old close times.
- Verify in PR: `[ingestion]` needs no `captive_core_config` with an injected core (`newCaptiveCoreOpeners` is not called). `e2e_test.go` `e2eConfigPath` sets it to `/dev/null`; copy that if validation needs it.

### 4.5 Freeze timing

- When ingestion crosses a chunk boundary, `lifecycle.Loop` freezes the completed chunk through `backfill.RunBackfill`. `backfill/execute.go` calls `cfg.metrics().Freeze(d)`, which records `soroban_rpc_fullhistory_streaming_phase_duration_seconds{phase="freeze"}` on the admin `/metrics`.
- So a `bench-live` run over 2 chunks gives one live freeze sample, measured while the node ingests and serves. The local test covers less than one chunk and does not check it.
- The tx-hash index width stays `geometry.ChunksPerTxhashIndex` (1,000), so no terminal index build runs in a 2-chunk run.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `P/daemon.go` | modify | `Options.Core`, `.Backend`, `.NetworkPassphrase`; `daemonOptions.networkPassphrase`; `resolveCore` | 20 |
| `P/bench/live_source.go` | new | `liveCore`, `liveStream`, `liveBackend` | 70 |
| `P/bench/live.go` | new | `NewLiveCommand`, flags, validation, config writer, `runLive` | 160 |
| `cmd/stellar-rpc/rpcv2/main.go` | modify | register `bench-live` | 1 |

## 6. Tests

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestBenchLive_ServesTransactionsWhileIngesting` | `P/bench/live_test.go` | While ingestion runs at the close interval, an HTTP client gets each new transaction with `getTransaction`. At least one `getTransaction` succeeds while `getLatestLedger` is below the last ledger. | `writeSourcePack(t, src, chunk.ID(0), 300)` (`bench_test.go`; a transaction every `eventEvery` = 100 ledgers); `runLive` with `--close-interval 20ms`, both listeners on `127.0.0.1:0`, addresses from `OnListen`; passphrase `network.PublicNetworkPassphrase` (the passphrase `rpcv2test.EventsLCMBytesAt` hashes with). A client loop polls `getLatestLedger`; for each new ledger with a transaction it calls `getTransactions` (start = that ledger, limit 1) and then `getTransaction` for the hash, over `rpcv2test.PostRPC`. |
| `TestBenchLive_HoldsHeadAfterLastLedger` | same | After the last pack ledger the node keeps serving at a static head; cancel gives a nil error. | same set-up; `getHealth` after the last ledger; cancel ctx. |
| `TestBenchLive_WrongPassphraseMisses` | same | With another passphrase, `getTransaction` for a known hash does not return `SUCCESS` (D20). | same set-up, `network.TestNetworkPassphrase`. |
| `TestBenchLive_RejectsBadFlags` | same | Empty `--network-passphrase` and an existing catalog fail before any write. | `NewLiveCommand()`, `cmd.SetArgs`. |
| `TestLiveStream_HonorsResumeAndHolds` | `P/bench/live_source_test.go` | `RawLedgers(UnboundedRange(s))` yields `[s, last]` and then blocks until ctx cancel. | `packBackend` over `writeSourcePack`; no daemon. |
| `TestRunDaemonWithOptions_InjectedPassphrase` | `P/daemon_test.go` | An injected `Core` with `Options.NetworkPassphrase` reaches `handlerParams.networkPassphrase`. | `fakeCore` (`P/startup_test.go`), `fakeBackend` (`P/helpers_test.go`), `writeTempConfig` (`P/daemon_test.go`); check via a `getNetwork` call. |

Run time: 300 ledgers at 20 ms is about 6 s, plus startup. The tests do not need `-short` skips.

## 7. Done when

- A local test ingests paced ledgers from a pack tree of 300 ledgers. At the same time an HTTP client gets each new transaction with `getTransaction`: `go test ./cmd/stellar-rpc/internal/rpcv2/bench -run TestBenchLive_ServesTransactionsWhileIngesting -race`.

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
go test -race ./cmd/stellar-rpc/internal/rpcv2 -run 'Daemon|E2E|Metrics'
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'
go run ./cmd/stellar-rpc/rpcv2 bench-live --help
```

Manual check: run `bench-live` over a 2-chunk pack tree with `--close-interval 0`, and read `phase_duration_seconds{phase="freeze"}` from `/metrics` after the boundary.

## 9. Risks and open points

- Check first: `backfillToTip` is a no-op when the tip is the first ledger of the earliest chunk. `e2e_test.go` shows the genesis case (tip = `FirstLedgerSeq + 5`). Add a unit check if the numeric case differs.
- Check first: `validateForm` passes the written config with no `[ingestion]` section.
- The daemon opens hot chunks read-write and runs the lifecycle, so `bench-live` changes its data directory. Never point it at a campaign dataset. The command help says so.
- A head that stops after the last ledger makes `getHealth` latency grow. `max_healthy_ledger_latency = "1000000h"` covers it.
- Each hot chunk has a 512 MB block cache. A 2-chunk run holds 2 caches until the lifecycle discards the frozen chunk.
- The pack stream needs the whole range in the tree (`TestRunHotIncompleteStream`). `--num-ledgers` must fit the tree; the stream reports it at the first pull.
- Decision to record in the decision log in this PR (next free D id, Proposed): `bench-live` refuses an existing catalog, and holds the head after the last ledger instead of exiting.
- `Options` is the integration-test harness API. The three new fields are optional and zero by default, so existing callers do not change.
