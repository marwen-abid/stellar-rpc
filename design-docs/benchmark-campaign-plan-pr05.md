# PR 05: bench-serve

| | |
|---|---|
| Branch | `bench-campaign-v2/05-bench-serve` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 01 (read-only hot open with events, `catalog.OpenReadOnly`), PR 02 (kept catalog, pinned earliest ledger, frontier chunk). PR 04 is optional: when it is merged first, pass its read sink. D30 is confirmed. |
| Implements | D3, D5 (file-set test over the dataset root), D20, D24, D26, D30; spec Sections 6.3 and 6.9; spec Section 10.1 row 05 |
| Estimate | About 390 non-test lines: `P/serve_dataset.go` 210, `P/jsonrpc.go` 15, `P/bench/serve.go` 110, `P/bench/profile.go` 45, `P/bench/command.go` 5, `cmd/stellar-rpc/rpcv2/main.go` 1 |

## 1. Goal

After this PR, `stellar-rpc-v2 bench-serve --dataset <root> --network-passphrase <p>` serves a dataset that `bench-ingest` wrote, over JSON-RPC, with no ingestion.
It uses the daemon's handlers, serves seven methods and returns -32601 for the others.
It opens every store read-only, so the dataset root does not change.
It serves `/metrics` and `/debug/pprof` on the admin port, with the request metric on the same registry.

## 2. Scope

- Add the exported entry point `rpcv2.ServeDataset(ctx, ServeDatasetOptions)` in new file `P/serve_dataset.go` (D26).
- Add an optional method allowlist to `handlerParams`, applied after `LimitsByMethod.Apply` in `newJSONRPCHandler` (`P/jsonrpc.go`).
- Add the cobra command `bench-serve` in new file `P/bench/serve.go`, and register it in `cmd/stellar-rpc/rpcv2/main.go`.
- Add the shared `--profile-rates` flag to `P/bench/profile.go`, bound by `bench-ingest cold`, `bench-ingest hot` and `bench-serve` (D24).
- Add the tests of Section 6, in new file `P/bench/serve_test.go`.

## 3. Out of scope (boundaries)

- The read-only hot open with events and `catalog.OpenReadOnly`: PR 01.
- The kept catalog, `PinEarliestLedger` and the frontier chunk: PR 02.
- The storage metrics: PR 04.
- Start, stop, readiness wait, cache drop and Blaster: PR 10 (the load step).
- `bench-live` (ingestion while serving): PR 13.
- A `dataset.json` manifest: rejected by D20.
- A copy of the method table in `bench`: rejected by D26.
- Any change to daemon startup or `query.OpenRegistry`: rejected by D3.

## 4. Design

### 4.1 Current code (verified at 91f158b)

- `newServeReads(cfg, params handlerParams) func(ctx, *query.Registry, net.Listener) error` (`P/serve.go`) fills `registry`, `ledgerReader` (`adapters.NewLedgerReader()`) and `transactionReader` (`adapters.NewTransactionReader(p.networkPassphrase, p.metrics)`), then calls `newJSONRPCHandler`. It drains on ctx cancel (`readShutdownTimeout` 5 s) and returns `ctx.Err()`.
- `startAdminServer(ctx, endpoint, logger, processRegistry) (net.Addr, func(), error)` (`P/serve.go`) serves `jsonrpc.NewAdminMux`.
- `newJSONRPCHandler(cfg, p)` (`P/jsonrpc.go`) calls `jsonrpc.BuildHandlerSpecs`, appends `queryEvents`, calls `limitsByMethod(m).Apply(specs)`, then wraps each spec with `wrapAdapterRequest` and `getHealth` with `gateHealthOnFirstCommit`. `BuildHandlerSpecs` calls `deps.Daemon.FastCoreClient()` at build time.
- At 91f158b the 13 methods are `getHealth`, `getEvents`, `getNetwork`, `getVersionInfo`, `getLatestLedger`, `getLedgers`, `getLedgerEntries`, `getTransaction`, `getTransactions`, `sendTransaction`, `simulateTransaction`, `getFeeStats` and `queryEvents` (`protocol.QueryEventsMethodName` = `"queryEvents"`; the facts digest calls it `getEventsV2`, from 36ac502).
- `runDaemonWith` (`P/daemon.go`) builds one `prometheus.NewRegistry()`, calls `host.RegisterProcessMetrics(registry, logger)`, then `buildSinks`. `host.Daemon` has `MetricsRegistry`, `MetricsNamespace`, `CoreClient`, `FastCoreClient` and `CoreVersion`. `host.MakeNoOpDaemon()` returns a `*host.NoOpDaemon`: its `FastCoreClient()` is a no-op client, and its `MetricsRegistry()` returns a new registry on each call (`internal/host/noOpDaemon.go`).
- `lastCommittedLedger(cat)` (`P/progress.go`) is unexported in `rpcv2`. It refines with `hotchunk.OpenReadyView` (read-only). `ServeDataset` is in `rpcv2`, so it calls it directly.
- `query.NewRegistry(cat, retention)` starts with latest 0 and `bootSeq` 0. So after `SetLatestLedger(latest > 0)`, `HasCommittedSinceBoot` is true and `gateHealthOnFirstCommit` passes.
- `adapters.SeedCloseTimes(registry)` is a no-op at latest 0 and fails when the retention floor is below the dataset (facts A2).
- `geometry.NewRetention(size uint32, earliestChunk chunk.ID)`, `Retention.RetentionWindow()`. `geometry.NewLayout(root)`, `Layout.CatalogPath()`, `Layout.HotChunkPath(c)`. `geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)`.
- `config.ParseConfig(nil)` fills every default. `ServiceConfig.Endpoint`, `.AdminEndpoint`, `.Methods.GetHealth.MaxHealthyLedgerLatency` (`*time.Duration`, default 30 s).

### 4.2 `rpcv2.ServeDataset`

```go
type ServeDatasetOptions struct {
	Dataset, Listen, AdminListen, NetworkPassphrase string
	Logger  *supportlog.Entry
	OnReady func(rpc, admin net.Addr)
}
func ServeDataset(ctx context.Context, opts ServeDatasetOptions) error
```

Sequence (spec 6.3). Each failure returns an error; the command exits non-zero.

1. Validate the options one time (code rule 3). An empty `NetworkPassphrase` fails before any open (D20). `Dataset`, `Listen` and `AdminListen` must be set.
2. `layout := geometry.NewLayout(opts.Dataset)`. Open the catalog with `catalog.OpenReadOnly(layout.CatalogPath(), layout, txLayout, logger)` (PR 01). It fails on a missing catalog and on a catalog without `meta/catalog-secret` (D36). `defer cat.Close()`.
3. `earliest, ok, err := cat.EarliestLedger()`. `!ok` fails with "dataset has no config:earliest_ledger". `retention := geometry.NewRetention(0, chunk.IDFromLedger(earliest))`.
4. `reg := query.NewRegistry(cat, retention)`. `defer reg.Close()` (it closes every published handle). Do not call `query.OpenRegistry`.
5. For each `c` in `cat.ReadyHotChunkKeys()`: open `layout.HotChunkPath(c)` with `hotchunk.OpenReadOnlyWithEvents` (PR 01), then `reg.PublishHandle(c, db)`. This includes the frontier chunk of a cold dataset.
6. `latest, err := lastCommittedLedger(cat)`, as the daemon does (D3). On a cold dataset the empty frontier chunk refines nothing, so `latest` is the last cold ledger. `reg.SetLatestLedger(latest, query.UnknownCloseTime())`. Then `adapters.SeedCloseTimes(reg)`: it reads the header of `latest` and calls `SetLatestLedger(latest, query.CloseTimeAt(<header close time>))`. So the registry holds the latest ledger with the close time of its header (spec 6.3 item 6).
7. `cfg, _ := config.ParseConfig(nil)`. Set `cfg.Service.Endpoint = opts.Listen`, `cfg.Service.AdminEndpoint = opts.AdminListen`, and `cfg.Service.Methods.GetHealth.MaxHealthyLedgerLatency` to `1000000h` (D3). The value 0 does not turn the check off: `validateService` rejects a limit below 1 ms. Test ledgers have close times near 1970, so the default 30 s gives an unhealthy `getHealth`.
8. `registry := prometheus.NewRegistry()`; `host.RegisterProcessMetrics(registry, logger)`; `metrics := observability.NewPrometheusMetrics(registry, host.PrometheusNamespace)`. With PR 04: `observability.NewReadMetrics(registry, host.PrometheusNamespace)`.
9. `coreDaemon := serveDaemon{NoOpDaemon: host.MakeNoOpDaemon(), registry: registry}`. `serveDaemon` is a small unexported type in `serve_dataset.go` (D3, spec 6.3 item 10). It embeds `*host.NoOpDaemon` and overrides `MetricsRegistry()` to return `registry`, the admin registry. `FastCoreClient()` comes from the embedded no-op daemon, so `BuildHandlerSpecs` gets a client at build time. No preflight pool: `simulateTransaction` is not served, and its handler does not use the getter at build time.
10. `adminAddr, stopAdmin, err := startAdminServer(ctx, cfg.Service.AdminEndpoint, logger, registry)`. `defer stopAdmin()`.
11. Listen on `cfg.Service.Endpoint` with `net.ListenConfig`. `defer listener.Close()`.
12. `serve := newServeReads(cfg, handlerParams{daemon: coreDaemon, logger, metrics, feeWindows: feewindow.NewFeeWindows(classic, soroban from cfg.Service.FeeStats), networkPassphrase: opts.NetworkPassphrase, retentionWindow: retention.RetentionWindow(), serveOnly: benchServeMethods})`.
13. Call `opts.OnReady(listener.Addr(), adminAddr)`. Then `err := serve(ctx, reg, listener)`. Map `context.Canceled` to nil, so SIGTERM gives exit 0 (spec 6.3 rules). The defers close the admin server, the listener, the hot handles and the catalog, in that order.

No captive core, ingestion, backfill or lifecycle starts. No fee replay runs, so `getFeeStats` would be empty; it is not served.

### 4.3 Method filter (`P/jsonrpc.go`)

- Add `serveOnly map[string]bool` to `handlerParams`. nil means all methods (the daemon).
- In `newJSONRPCHandler`, after `specs = limitsByMethod(m).Apply(specs)`: `if p.serveOnly != nil { specs = slices.DeleteFunc(specs, func(s jsonrpc.HandlerSpec) bool { return !p.serveOnly[s.MethodName] }) }`. `Apply` sees the full table, so it does not panic.
- `benchServeMethods` (unexported, in `serve_dataset.go`): the seven `protocol.*MethodName` constants of D30. jrpc2 answers other names with -32601 (facts S16).
- `methods_completeness_test.go` is not affected: the daemon passes nil.

### 4.4 Command (`P/bench/serve.go`)

```
stellar-rpc-v2 bench-serve --dataset /mnt/nvme/bench/sac-6000/run1/cold \
  --listen 127.0.0.1:8000 --admin-listen 127.0.0.1:8001 \
  --network-passphrase "<passphrase of the packs>" --profile-rates 0,0
```

- `func NewServeCommand() *cobra.Command`. `cmd/stellar-rpc/rpcv2/main.go`: `rootCmd.AddCommand(bench.NewServeCommand())` next to `bench.NewCommand()`.
- Flags: `--dataset` (required), `--listen` (default `127.0.0.1:8000`), `--admin-listen` (default `127.0.0.1:8001`), `--network-passphrase` (no default), `--profile-rates`.
- `RunE` uses `benchContext()` (SIGINT/SIGTERM context, Info logger), applies the profile rates, and calls `rpcv2.ServeDataset`. `OnReady` writes one line `ready` to `cmd.OutOrStdout()`. The runner does not parse it; it polls `getHealth` (spec 6.7, code rule 6).

### 4.5 `--profile-rates` (`P/bench/profile.go`)

- New `type rateFlags struct{ spec string }`, `bind(cmd)` with default `"0,0"`, and `apply() error`. It parses `<block ns>,<mutex fraction>` as two non-negative ints and calls `runtime.SetBlockProfileRate` and `runtime.SetMutexProfileFraction`. A bad value fails before any open.
- `newBenchCommand` (`P/bench/command.go`) binds it for `cold` and `hot`. `bench-serve` binds it. PR 13 binds it for `bench-live`. The existing `profileFlags` (`--cpuprofile`, `--memprofile`) stays on `bench-ingest` only; `bench-serve` has `/debug/pprof`.

### 4.6 Size check

The estimate is about 390 lines, under the 600-line limit. A split is not necessary. If the implementation grows past 600, split it as 05a (`ServeDataset`, the filter, the file-set test) and 05b (the command, `--profile-rates`, `main.go`).

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `P/serve_dataset.go` | new | `ServeDatasetOptions`, `ServeDataset`, `serveDaemon`, `benchServeMethods`, open and publish loop | 210 |
| `P/jsonrpc.go` | modify | `handlerParams.serveOnly`, filter after `Apply` | 15 |
| `P/bench/serve.go` | new | `NewServeCommand`, flags, `ready` line | 110 |
| `P/bench/profile.go` | modify | `rateFlags` | 45 |
| `P/bench/command.go` | modify | bind `rateFlags` in `newBenchCommand` | 5 |
| `cmd/stellar-rpc/rpcv2/main.go` | modify | register `bench-serve` | 1 |

## 6. Tests

Put the tests in package `bench` (`P/bench/serve_test.go`), so they can use the `bench_test.go` helpers. Add one helper there: `buildServeDataset(t, tier string) (root string)`.

- Cold: `writeSourcePack(t, src, chunk.ID(0), chunk.LedgersPerChunk)` (every `eventEvery` = 100th ledger carries one transaction and one contract event), then `runCold` with `ColdRoot: root`. After PR 02 the root holds the catalog, the pinned earliest ledger and the ready frontier chunk 1.
- Hot: `writeSourcePack(t, src, chunk.ID(0), 300)`, then `runHot` with `HotRoot: root, NumLedgers: 300`.
- Start `rpcv2.ServeDataset` in a goroutine on `127.0.0.1:0` for both listeners. Get the addresses from `OnReady`. Call over HTTP with `rpcv2test.PostRPC(t, url, method, params)`.

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestBenchServe_ServesMethods/cold`, `/hot` | `P/bench/serve_test.go` | Each of the seven methods answers without error. `getTransactions` returns hashes. `getTransaction` for one of them returns `SUCCESS`. `getEvents` returns at least one event. `getHealth` reports `healthy` with the dataset's oldest and latest ledgers. | `buildServeDataset`, `rpcv2test.PostRPC`. |
| `TestBenchServe_OtherMethodsNotFound` | same | `getFeeStats`, `getVersionInfo`, `getLedgerEntries`, `sendTransaction`, `simulateTransaction`, `queryEvents` return error code -32601. | hot dataset; `RPCResponse.Error`. |
| `TestBenchServe_DatasetUnchanged/cold`, `/hot` | same | The dataset root file set (relative path, size, modification time) is the same before the start and after serve, reads (events included) and cancel. | `fileset.Take` before the start, `fileset.RequireUnchanged` after the cancel (`P/rpcv2test/fileset`, PR 01). |
| `TestBenchServe_MetricsOnAdmin` | same | `/metrics` holds `soroban_rpc_json_rpc_request_duration_seconds` for `getLatestLedger` after one call, and `go_goroutines`. This proves that `serveDaemon.MetricsRegistry()` is the admin registry. | HTTP GET, as `scrapeMetrics` in `P/metrics_test.go` does. |
| `TestBenchServe_EmptyPassphraseFails` | same | `bench-serve --dataset <root>` with no `--network-passphrase` returns an error before it opens anything; the dataset root is unchanged. | `NewServeCommand()`, `cmd.SetArgs`, `cmd.Execute()`. |
| `TestBenchServe_ReadyLine` | same | The command writes `ready` to stdout once, and exits 0 on cancel. | `cmd.SetOut(&buf)`, `cmd.ExecuteContext(ctx)`. |
| `TestProfileRates_Parse` | `P/bench/profile_test.go` | `0,0`, `1000,5` pass; `-1,0`, `abc`, `1` fail. | table test. |
| `TestJSONRPCHandler_ServeOnlyFilter` | `P/jsonrpc_test.go` | With `serveOnly` set, only those methods answer; with nil, all 13 answer. | `seedServingRegistry`, `testHandlerParams`, `newTestRPCServer`. |

## 7. Done when

- A test calls each served method over HTTP on a cold and a hot test dataset: `go test ./cmd/stellar-rpc/internal/rpcv2/bench -run TestBenchServe_ServesMethods`.
- Other methods return -32601: `-run TestBenchServe_OtherMethodsNotFound`.
- The file-set test over the dataset root passes: `-run TestBenchServe_DatasetUnchanged`.
- An empty `--network-passphrase` fails at start: `-run TestBenchServe_EmptyPassphraseFails`.

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/... 
go test -race ./cmd/stellar-rpc/internal/rpcv2 -run 'JSONRPC|Serve|Metrics|MethodsCompleteness'
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'
go run ./cmd/stellar-rpc/rpcv2 bench-serve --help
```

Manual check on a local dataset from `bench-ingest cold`: start `bench-serve`, run `curl` for `getHealth`, send SIGTERM, check exit code 0.

## 9. Risks and open points

- Check first: the signatures that PR 01 merges (`catalog.OpenReadOnly`, `hotchunk.OpenReadOnlyWithEvents`).
- Check first: PR 02 changes `runCold` and `runHot` options. `buildServeDataset` must use the merged names.
- `lastCommittedLedger` opens the highest ready hot chunk a second time, read-only, while `ServeDataset` holds its own read-only handle. Two read-only opens take no LOCK and write nothing (facts S1). The file-set test covers it.
- Each opened hot chunk has a 512 MB block cache (facts). A hot dataset of 2 chunks plus nothing else is about 1 GB. A cold dataset opens one empty frontier chunk.
- The events warmup of each hot chunk runs at open (facts A1). On a full chunk it can take long. PR 10 waits on `getHealth`, not on the `ready` line, so this is safe; note the time in the PR description.
- `observability.NewPrometheusMetrics` registers gauges that nothing sets in `bench-serve` (for example `last_committed_ledger`). They read 0. That is acceptable; say so in the command help.
- `getHealth` (`internal/methods/get_health.go`) does not check the retention window. It reports `ledgerRetentionWindow` = `RetentionWindow()` = 0 (full history), as the daemon does with `retention_chunks = 0`. Test LCMs can have close time 0 (1970). `1000000h` (about 114 years) covers it.
- `serveDaemon` must override `MetricsRegistry()`. Without the override, the embedded `NoOpDaemon` returns a new registry on each call, and the request metric goes to a registry that `/metrics` does not serve. `TestBenchServe_MetricsOnAdmin` catches it.
- A read-only open beside a writer is undefined in RocksDB. Nothing enforces it. The command help says: never run `bench-serve` on a dataset that another process writes.
