# PR 04: Storage metrics in the shared read path

| | |
|---|---|
| Branch | `bench-campaign-v2/04-storage-metrics` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | Nothing. It can go in parallel with PRs 02, 03 and 06. |
| Implements | D6 (amended), D35; spec Section 6.4, spec Section 10.1 row 04 |
| Estimate | About 370 non-test lines: `query/read_timing.go` 170, `query/registry.go` 35, `query/resolve.go` 25, `query/tx_lookup.go` 25, `stores/ledger/cold_reader.go` 12, `stores/event/cold_reader.go` 18, `observability/read_metrics.go` 60, `jsonrpc.go` 12, `daemon.go` 10. Docs are not counted. |

## 1. Goal

After this PR, each JSON-RPC request records the time it spent in each store (`ledgers`, `events`, `txhash`) and tier (`hot`, `cold`).
The daemon exports two histograms on the admin `/metrics`: `soroban_rpc_fullhistory_read_store_seconds{method,store,tier}` and `soroban_rpc_fullhistory_read_open_seconds{store}`.
Production and `bench-serve` (PR 05) use the same code path.
A read view without a method name records nothing.

## 2. Scope

- Add the timers, the per-view accumulator and the sink interface in package `query` (new file `query/read_timing.go`).
- Return the tier from `resolveLedgers` and from `ReadView.Events` through a new `resolveEvents` (`query/resolve.go`), and give `windowGatedIndex` a tier (`query/tx_lookup.go`).
- Measure the blocking first-use wait in the lazy cold readers (`stores/ledger/cold_reader.go`, `stores/event/cold_reader.go`).
- Add the Prometheus sink `observability.ReadMetrics` (new file `observability/read_metrics.go`) and wire it through `wrapAdapterRequest` (`jsonrpc.go`) and `runDaemonWith` (`daemon.go`).
- Document both metrics in `docs/MONITORING.md` and in the metrics table of `docs/ARCHIVE-NODE-BETA-RUNBOOK.md`.

## 3. Out of scope (boundaries)

- `bench-serve` and its admin registry: PR 05. PR 05 passes the same sink.
- The storage share calculation from two `/metrics` dumps: PR 10 (runner) and B3 (converter).
- A reader cache for cold readers. Cold readers stay per call (spec 6.4).
- An isolated storage benchmark: rejected by D7.
- Changes to `soroban_rpc_json_rpc_request_duration_seconds` (a Summary). It stays as it is.

## 4. Design

### 4.1 Where the time goes today (verified at 91f158b)

- `ReadView.Ledgers(c)` and `ReadView.WithLedger(seq, fn)` (`query/resolve.go`) call `resolveLedgers(c)`, which returns `(LedgerReader, func() error, error)` and no tier. `ScanLedgers` (`query/ledger_scan.go`) gets its readers from `ledgerWalk.open`, which also calls `resolveLedgers`.
- `ScanLedgers` returns an `iter.Seq2[ledger.Entry, error]`. Its loop calls `r.IterateLedgers(from, to)` and then `yield(e, ierr)`. The consumer body (for example `adapters.ledger_reader.go` or `feereplay.go`) runs inside that `yield`.
- `ReadView.Events(c)` returns an `event.Reader` and no tier. The event store work happens later, in `scanChunk` and `event.Matches` (`query/event_page.go`), through the `event.Reader` methods `Offsets`, `EventCount`, `LookupKeys`, `FetchEvents`, `FetchRange`, `All`.
- `HotTxHashIndexes` and `ColdTxIndexes` (`query/tx_lookup.go`) wrap each index in `windowGatedIndex{inner, view}`. `lazyColdTxIndex.Get` calls `txhash.OpenColdReader` on the first `Get`. That open is synchronous.
- `ledger.OpenColdReader` and `event.OpenColdReader` do no I/O. The ledger reader blocks in `c.init()` (a `sync.OnceValues` over `loadHeader`). The event reader blocks in `waitMeta` and in `validateMPHF` (which waits on `waitMPHF`).
- `Registry.NewReadView()` takes no argument. Callers: `wrapAdapterRequest` (`jsonrpc.go`), `replayFeeWindows` (`feereplay.go`), `adapters.SeedCloseTimes` (`adapters/seed.go`).
- `newJSONRPCHandler` has `specs[i].MethodName` in the loop that calls `wrapAdapterRequest(specs[i].Handler, p.registry)`. The request metric labels `endpoint` with `r.Method()`, which is the same string.

### 4.2 Sink and accumulator (package `query`)

`query` cannot import `observability` (`observability` imports `query`). So `query` defines the sink:

```go
type ReadSink interface {
	ObserveStore(method string, store geometry.Kind, tier string, d time.Duration)
	ObserveOpen(store geometry.Kind, d time.Duration)
}
```

- The store label is the `geometry.Kind` string: `ledgers`, `events`, `txhash` (`geometry/keys.go`).
- The tier label is `hot` or `cold`. Add `func (t tier) label() string` beside the existing `tierCold`, `tierHot`.
- New constructor (D35): `func (r *Registry) NewRequestView(method string, sink ReadSink) (*ReadView, error)`. It calls `NewReadView` and sets two new `ReadView` fields, `method` and `sink`. `NewReadView()` stays unchanged, so `feereplay.go` and `adapters/seed.go` get a view that records nothing.
- The sink type that the daemon and `bench-serve` pass is `observability.ReadMetrics` (4.5, D35). `ReadSink` is only the interface that `query` needs to avoid the import cycle.
- New `ReadView` field `storeTimes [3][2]storeTime` with `storeTime{d time.Duration; used bool}`. Index 0..2 = ledgers, events, txhash. Index 0..1 = hot, cold. A view serves one goroutine, so no lock is necessary (see the `ReadView` doc comment).
- `ReadView.timed() bool` is true when `method != ""` and `sink != nil`. When it is false, the resolve methods return the raw readers. Views without a method pay no timer cost.
- `ReadView.Release` runs the closers first (they can add open time), then calls `sink.ObserveStore` one time for each `(store, tier)` with `used == true`, then releases the snapshot. The existing double-release guard (`a.snap == nil`) also guards the observation.

### 4.3 Timer points

| Point | Change | What is timed |
|---|---|---|
| `resolveLedgers` | Return `(LedgerReader, tier, func() error, error)`. When `timed()`, wrap the reader in `timedLedgerReader{inner, view, tier}`. | See next rows. |
| `timedLedgerReader.WithLedger(seq, fn)` | Time the call. Subtract the time spent inside `fn`. | The store read, not the handler's decode in `fn`. |
| `timedLedgerReader.IterateLedgers(start, end)` | Wrap the inner iterator. Start a clock before each step. Stop it when the inner iterator calls the wrapper's yield. Restart it after the consumer's `yield` returns. | Each iterator step only. The body of `ScanLedgers`' consumer is not counted. |
| `ReadView.Events` | Move the tier switch to `resolveEvents(c) (event.Reader, tier, error)`, which `Events` calls. `Events` keeps its signature; its one caller is `event_page.go`. When `timed()`, wrap in `timedEventReader`. | Each of `Offsets`, `EventCount`, `LookupKeys`, `FetchEvents`; each step of `FetchRange` and `All`, with the same yield rule. `ChunkID` is not timed. |
| `windowGatedIndex.Get` | Add a field `tier`. `HotTxHashIndexes` sets `tierHot`, `ColdTxIndexes` sets `tierCold`. Time `g.inner.Get(hash)` when `timed()`. | The hot index get, or the cold `.idx` open plus get. |
| `lazyColdTxIndex.Get` | Time `txhash.OpenColdReader(...)` and call `sink.ObserveOpen(geometry.KindTxHash, d)` when `timed()`. | The synchronous `.idx` open. It also counts in the store time through `windowGatedIndex.Get`. |
| Cold ledger and event readers | At close time, read the first-use wait and call `sink.ObserveOpen`. See 4.4. | The open cost at the first blocking call. |

`ScanLedgers` itself needs no timer. Its readers come from `resolveLedgers`, so the steps are already timed. `WithLedger` also needs no timer of its own, for the same reason.

### 4.4 Open time for the lazy cold readers

The open cost lands at the first blocking call, not at `OpenColdReader` (D6, D35, spec 6.4). The plan:

- `stores/ledger/cold_reader.go`: add `openWait atomic.Int64` and `opened atomic.Bool` to `ColdReader`. Time the body of `loadHeader` (it waits on the background open through `c.r.Trailer()`). Add `func (c *ColdReader) OpenWait() (time.Duration, bool)`.
- `stores/event/cold_reader.go`: add the same two fields. Add the elapsed time of the first `waitMeta` loader and of the first `validateMPHF` gate. Add `OpenWait() (time.Duration, bool)`.
- In `query`, when `timed()`, wrap the reader's close function. The wrapper calls `OpenWait()`. If the reader did a blocking first use, it calls `sink.ObserveOpen(store, d)`. So one reader gives at most one open observation. A one-ahead reader that `ScanLedgers` opened but never read gives none.
- The open wait is inside the first read, so it also counts in `fullhistory_read_store_seconds` (spec 6.4).

D35 sets this rule: the open time is measured at the first blocking call (`init`, `waitMeta`, the MPHF load) and observed once, when the reader closes. So each cold reader gives at most one observation.

### 4.5 Prometheus sink (`observability/read_metrics.go`)

```go
func NewReadMetrics(registry *prometheus.Registry, namespace string) *ReadMetrics
func (m *ReadMetrics) ObserveStore(method string, store geometry.Kind, tier string, d time.Duration)
func (m *ReadMetrics) ObserveOpen(store geometry.Kind, d time.Duration)
```

- Namespace `host.PrometheusNamespace` (`soroban_rpc`). Subsystem `fullhistory_read`. Names `store_seconds` and `open_seconds`. Full names (D35, spec 6.4): `soroban_rpc_fullhistory_read_store_seconds`, `soroban_rpc_fullhistory_read_open_seconds`.
- It is a separate type, not new methods on `observability.Metrics`. That interface has test implementations in `helpers_test.go`, `backfill/recorder_test.go` and `e2e_test.go`.
- Bucket layout (D35): `prometheus.ExponentialBuckets(0.000025, 4, 10)` for both histograms: 25 µs, 100 µs, 400 µs, 1.6 ms, 6.4 ms, 25.6 ms, 102 ms, 410 ms, 1.64 s, 6.55 s. Reason: a hot RocksDB get is tens of µs; a cold getEvents page with opens is ms to s; factor 4 matches `phaseBuckets`. Series for the store histogram: 13 × 3 × 2 × (10 buckets + `+Inf` + `_sum` + `_count`) = 1,014 at most. Only used pairs appear.
- `phaseBuckets` stays for `phase_duration_seconds` only.

### 4.6 Wiring

- `jsonrpc.go`: add `readSink query.ReadSink` to `handlerParams`. Change `wrapAdapterRequest(h, registry)` to `wrapAdapterRequest(h, registry, method string, sink query.ReadSink)`. It calls `registry.NewRequestView(method, sink)`. The loop passes `specs[i].MethodName` and `p.readSink`.
- `daemon.go` `runDaemonWith`: build `observability.NewReadMetrics(registry, host.PrometheusNamespace)` next to `buildSinks`, and set `readSink` in the `handlerParams` given to `newServeReads`.
- A nil `readSink` (tests that use `testHandlerParams`) records nothing.

### 4.7 Documentation

- `docs/MONITORING.md`: add item 7 under `## Monitoring Metrics`, in the existing numbered format. Give both names, the labels, the storage share formula of spec 6.4, and the timeout rule: after a duration-limiter timeout the handler keeps running, so its storage observation can arrive after the response.
- `docs/ARCHIVE-NODE-BETA-RUNBOOK.md`: add two rows to the table under `### Key Metrics to Monitor`, after `soroban_rpc_json_rpc_request_duration_seconds`.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `P/query/read_timing.go` | new | `ReadSink`, `storeTime`, `timedLedgerReader`, `timedEventReader`, close wrapper, `tier.label` | 170 |
| `P/query/registry.go` | modify | `ReadView.method`, `.sink`, `.storeTimes`; `NewRequestView`; observation in `Release` | 35 |
| `P/query/resolve.go` | modify | tier from `resolveLedgers`, new `resolveEvents`, wrapping; remove nothing else | 25 |
| `P/query/tx_lookup.go` | modify | `windowGatedIndex.tier`, timed `Get`, `.idx` open observation | 25 |
| `P/stores/ledger/cold_reader.go` | modify | `openWait`, `opened`, `OpenWait()` | 12 |
| `P/stores/event/cold_reader.go` | modify | `openWait`, `opened`, `OpenWait()` | 18 |
| `P/observability/read_metrics.go` | new | `ReadMetrics`, buckets, two histograms | 60 |
| `P/jsonrpc.go` | modify | `handlerParams.readSink`, `wrapAdapterRequest` signature | 12 |
| `P/daemon.go` | modify | build and pass `ReadMetrics` | 10 |
| `docs/MONITORING.md`, `docs/ARCHIVE-NODE-BETA-RUNBOOK.md` | modify | metric documentation | not counted |

## 6. Tests

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestReadView_OneObservationPerStoreAndTier` | `P/query/read_timing_test.go` | A request that reads hot ledgers, hot txhash and cold ledgers gives exactly 3 `ObserveStore` calls with the right labels. | `newTestRegistry`, `publishReadyHandle`, `seedHotLedgers`, `rpcv2test.WriteFrozenLedgerPackSeq`; a recording `ReadSink` in the test file. |
| `TestReadView_NoMethodRecordsNothing` | same | `NewReadView()` and `NewRequestView("", sink)` give zero calls. `adapters.SeedCloseTimes` records nothing. | same helpers; recording sink. |
| `TestScanLedgers_SlowYieldNotCounted` | same | A consumer that sleeps 5 ms per entry over 20 hot ledgers adds less than 20 ms to `ledgers/hot`. | `seedHotLedgers` over one chunk; `time.Sleep` in the loop body; recording sink. |
| `TestWithLedger_SlowCallbackNotCounted` | same | Time spent in `fn` is not counted. | same. |
| `TestColdGetEvents_RecordsOpen` | `P/jsonrpc_test.go` | One cold `getEvents` page gives one `open_seconds{store="events"}` sample with a value above 0, and `store_seconds{method="getEvents",store="events",tier="cold"}` count 1. | New helper `rpcv2test.WriteFrozenEventsChunk(t, cat, c, lcms iter.Seq[[]byte])`: `ingest.WriteColdChunk` with `ingest.Config{Ledgers: true, Events: true, EventsSecret}` over a full chunk (`rpcv2test.EventsLCMBytes` at a few seqs, `rpcv2test.ZeroTxLCMBytes` elsewhere), then `cat.FlipChunkFrozen` for both kinds; a ready empty hot chunk above it (`rpcv2test.SeedHotChunkLCMs`); `newJSONRPCHandler` with a real `ReadMetrics`; read with `prometheus/testutil`. |
| `TestColdGetTransaction_RecordsTxhashOpen` | `P/jsonrpc_test.go` | A cold `getTransaction` gives one `open_seconds{store="txhash"}` sample. | `rpcv2test.WriteColdTxIndexFile`, `freezeCoverage`, `freezeKinds` (helpers_test.go). |
| `TestReadMetrics_Names` | `P/observability/read_metrics_test.go` | The two full metric names and the label sets. | `testutil.CollectAndCount`, `testutil.ToFloat64`. |
| `BenchmarkReadTiming_AccumulateAndObserve` | `P/observability/read_metrics_test.go` | The cost of one timer pair plus one `ObserveStore`. | `b.Loop()`; report ns/op in the PR description. |
| `BenchmarkTimedLedgerReader_WithLedger` | `P/query/read_timing_test.go` | The timed hot `WithLedger` against the raw one. | `seedHotLedgers`; `b.Run("raw")`, `b.Run("timed")`. |

## 7. Done when

- One request records one observation for each store and tier it used: `go test ./cmd/stellar-rpc/internal/rpcv2/query -run TestReadView_OneObservationPerStoreAndTier`.
- A view with no method name records nothing: `-run TestReadView_NoMethodRecordsNothing`.
- A microbenchmark gives the cost of one accumulate-and-observe pair: `go test ./cmd/stellar-rpc/internal/rpcv2/observability -run '^$' -bench BenchmarkReadTiming_AccumulateAndObserve`. Put the ns/op in the PR description.
- A test with a slow yield body shows no change in `fullhistory_read_store_seconds`: `-run 'TestScanLedgers_SlowYieldNotCounted|TestWithLedger_SlowCallbackNotCounted'`.
- A cold `getEvents` page records a non-zero open observation: `go test ./cmd/stellar-rpc/internal/rpcv2 -run TestColdGetEvents_RecordsOpen`.

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/stellar-rpc/internal/rpcv2/query/... ./cmd/stellar-rpc/internal/rpcv2/observability/... \
  ./cmd/stellar-rpc/internal/rpcv2/stores/ledger/... ./cmd/stellar-rpc/internal/rpcv2/stores/event/... \
  ./cmd/stellar-rpc/internal/rpcv2/adapters/... ./cmd/stellar-rpc/internal/rpcv2/eventsapi/...
go test -race ./cmd/stellar-rpc/internal/rpcv2 -run 'JSONRPC|Cold|Metrics|WrapAdapter'
go test ./cmd/stellar-rpc/internal/rpcv2/query -run '^$' -bench . -benchtime 2s
make go-check-branch BASE=feature/full-history
git diff --stat feature/full-history -- . ':!*_test.go' ':!*.md'
```

## 9. Risks and open points

- Check first: `event.Matches` (`stores/event/match.go`) takes the `event.Reader` interface and does its own CPU work between reader calls. Only the reader calls are timed. State this in the metric help text.
- Check first: no code asserts the concrete type of the reader that `ReadView.Events` or `ReadView.Ledgers` returns. A search of `query`, `adapters` and `eventsapi` at 91f158b found none. Search again on the PR base.
- `time.Now` per iterator step costs about 20 to 40 ns. A `getLedgers` request reads at most one chunk (`walkSpanCap` = 10,000). The benchmark gives the real number.
- A timed-out request keeps running and observes at `Release`, after the response. The storage share for such a method can read above 100%. Document it (4.7).
- D35 fixes the metric names, the bucket layout, `NewRequestView`, the sink type and the open observation point (4.4). A change to any of them needs an amendment of D35 first.
- `rpcv2test.WriteFrozenEventsChunk` ingests a full chunk of 10,000 ledgers. `backfill/process_test.go` `writeRealPack` does the same, so the run time is acceptable. Skip it in `-short` if it is slow.
- PR 05 reuses `handlerParams.readSink`. If PR 05 lands first, PR 04 adds the field to `ServeDataset` too.
