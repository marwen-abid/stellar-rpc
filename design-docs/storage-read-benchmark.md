# Storage Read Benchmark (`bench-read`)

| | |
|---|---|
| Status | Draft |
| Branch | `bench-read/00-spec`, from `feature/full-history` at `36ac502` |
| Replaces | [marwen-abid/stellar-rpc#12](https://github.com/marwen-abid/stellar-rpc/pull/12), [marwen-abid/stellar-rpc#13](https://github.com/marwen-abid/stellar-rpc/pull/13), [marwen-abid/stellar-rpc#14](https://github.com/marwen-abid/stellar-rpc/pull/14) (`bench-query`) |
| Related | [stellar-rpc-blaster](https://github.com/stellar/stellar-rpc-blaster) (branch `dev`), [stellar-rpc-benchmarks](https://github.com/stellar-experimental/stellar-rpc-benchmarks) |

## 1. Summary

This document specifies the rework of the full-history read benchmarks. The
rework gives each question to one tool:

- **Blaster** measures endpoint latency and capacity. It sends JSON-RPC
  requests over HTTP to a server that serves a benchmark dataset.
- **`bench-read`** measures the cost of storage reads, per tier, in the
  process. It does not model load.

The rework also adds storage-read metrics to the daemon. With these metrics,
a Blaster run shows the storage share of each request.

## 2. Background

### 2.1 The `bench-query` stack

PRs #12, #13 and #14 add `bench-query cold|hot`. The command opens a dataset
that `bench-ingest` made. It sends paced requests to `query.ReadView` at fixed
arrival rates. The stack has these problems:

1. `bench-query hot` opens the chunk database read-write. WAL replay and
   compaction change the dataset. A second run measures a different database.
2. It runs one query type at a time. It cannot measure a mixed workload.
3. It has no ramp. The operator must guess the rates. Its in-flight cap (512)
   does not match the daemon's per-method limits.
4. The percentiles exclude shed requests. Under overload, the percentiles
   improve when the load increases.
5. The `events` requests use 4 filter sets, all for busy contracts. All
   results go into one row.
6. The cold fixture rebuilds the catalog state by hand. It does not use the
   daemon's open sequence.
7. The CSV stores rates in nanosecond columns. The README has about 20 rules
   that a reader must know before reading one report.
8. The code quality is low. Section 11 gives examples.

About half of the code is a load generator. Blaster already is a load
generator, and a better one.

### 2.2 Blaster

Blaster (`stellar/stellar-rpc-blaster`, branch `dev`) is an HTTP load tester
for Stellar RPC. It uses vegeta.

- It sends a mixed workload by default. `--serial` sends one endpoint at a
  time.
- It ramps the rate in steps. It writes a timeline per step, which shows the
  rate where the server degrades.
- Its request shapes come from production traffic. `getEvents` uses 7
  weighted archetypes (head-poll, deep-pager, deep-scan and others), and the
  results show each archetype. `getLedgers` and `getTransactions` pick start
  ledgers across the full retention window.
- Its `generate` command collects seed data from the server.

Blaster has one known limit. vegeta measures latency from the send time and
caps its workers. Near saturation, Blaster can hide queue delay.

### 2.3 The campaign runner uses `bench-query`

`stellar-experimental/stellar-rpc-benchmarks` runs the campaigns:

- `runner/internal/plan/plan.go` builds the `bench-query` command lines. It
  uses one leg per query type, with `--target-rps` rate ladders from
  `docs/targets.json`.
- `converter/convert.py` reads the paced rows (`total_r<rate>`,
  `<qtype>_r<rate>_millirps`, `_lag`, `_shed`). It attaches SLA verdicts per
  endpoint and tier.

The SLA verdict is an endpoint verdict. But `bench-query` measures storage
only, one endpoint at a time. `docs/targets.json` records this limit: "A leg
drives one endpoint at a time, so the mix rates apply per endpoint in
sequence, not as one blended 500 rps stream." Blaster can send the blended
stream. This rework moves the SLA verdicts to Blaster runs (Section 10).

The converter also has an older closed-loop path (`build_queries`). It reads
`total_c<W>` rows and `<qtype>_c<W>` wall rows. `bench-read` uses this format.

### 2.4 Facts from the current code

These facts come from `feature/full-history` at `36ac502`.

- **Handlers.** RPCv2 uses the shared `methods` handlers.
  `wrapAdapterRequest` (`rpcv2/jsonrpc.go`) gets one read view per request and
  releases it at the end.
- **Tier choice.** `ReadView.resolveTier` (`query/resolve.go`) selects hot or
  cold for each chunk.
- **Cold reader cost.** A read view opens each cold reader that it needs
  (`ledger.OpenColdReader`, `event.OpenColdReader`, `txhash.OpenColdReader`).
  `ReadView.Release` closes them. Thus each cold request includes the cost to
  open its readers.
- **No dataset catalog.** `bench-ingest` deletes its catalog when it exits
  (`bench/scratch.go`, `openScratchCatalog`). The daemon cannot open a
  benchmark dataset.
- **Secrets.** Only `backfill/process.go` calls `Catalog.Secret`. The read
  path gets the index secret from the file metadata. A new catalog can read an
  existing dataset.
- **Read-only hot open.** `hotchunk.OpenReadOnly` makes no events facade. The
  events warmup runs only on a read-write open. A comment in
  `stores/hotchunk/hotchunk.go` gives this work to #772 ("a warmed read-only
  variant").
- **No serve-only mode.** Daemon startup needs captive core and a backfill
  backend. It runs `backfillToTip` before it serves (`startup.go`, `run`).
- **Metrics.** The shared JSON-RPC package records
  `json_rpc_request_duration_seconds`: a Summary with the labels `endpoint`
  and `status`. No metric measures storage reads. `docs/MONITORING.md` does
  not list RPCv2 metrics.

## 3. Goals and non-goals

### 3.1 Goals

| ID | Goal |
|---|---|
| G1 | Measure the service time of each storage read operation, per tier, on a fixed dataset. |
| G2 | Make runs repeatable. The same dataset and the same corpus give the same requests. A run does not change the dataset. |
| G3 | Define the cache state of each measurement (warm or evicted), and verify it. |
| G4 | Let Blaster run against a benchmark dataset, so the campaign gets end-to-end numbers under a mixed load. |
| G5 | Show the storage share of each request in a daemon under load. |
| G6 | Keep the converter's CSV format, so the campaign pipeline needs only small changes. |

### 3.2 Non-goals

- `bench-read` does not model arrival rates, load, capacity or SLAs. Blaster
  does these.
- `bench-read` does not measure handlers, JSON encoding or HTTP.
- This work does not change Blaster.

## 4. Name

Use **`bench-read`**, with the subcommands `cold` and `hot`.

| Name | Decision | Reason |
|---|---|---|
| `bench-read` | Selected | It pairs with `bench-ingest`: ingest is the write path, read is the read path. The tier subcommands are the same. |
| `bench-storage` | Rejected | `bench-ingest` also measures storage. The name does not say which half it measures. |
| `bench-query` | Rejected | "Query" suggests RPC queries. Blaster measures those. |
| `bench storage ingest\|read` | Rejected | It changes the `bench-ingest` command line that the campaign runner uses. |

The dataset server in Section 7 is **`bench-serve`**.

## 5. Work items

| ID | Work | Repository | Depends on |
|---|---|---|---|
| W1 | Read-only hot open with an events facade | stellar-rpc | — |
| W2 | Durable dataset catalog from `bench-ingest` | stellar-rpc | — |
| W3 | `bench-read` | stellar-rpc | W1, W2 |
| W4 | `bench-serve` | stellar-rpc | W1, W2 |
| W5 | Storage-read metrics in the daemon | stellar-rpc | — |
| R1 | Runner and converter support for `bench-read` | stellar-rpc-benchmarks | W3 |
| R2 | Blaster legs, and SLA verdicts from Blaster | stellar-rpc-benchmarks | W4, W5 |

## 6. Datasets

### 6.1 W1: Read-only hot open with an events facade

Requirements:

1. Add one function to `stores/hotchunk` that opens a chunk database
   read-only and builds the events facade. The facade must use the same
   warmup as the read-write open (`event.NewWithStore`).
2. The open must not write to the database directory.
3. The registry must accept the handle through `Registry.PublishHandle`.
4. The events warmup must not write to the store. Prove this with a test on
   a read-only store.
5. The `hotchunk.go` comment gives this variant to #772. Tell the owner of
   #772 about this work before the PR.

Acceptance:

- A test opens a hot database with the new function, runs ledger, tx-hash
  and events reads, and closes it.
- Before and after this test, the file set of the directory is the same:
  names, sizes and modification times.

### 6.2 W2: Durable dataset catalog

Requirements:

1. `bench-ingest cold` and `bench-ingest hot` write the catalog at
   `Layout.CatalogPath` (`<root>/catalog/rocksdb`). They keep the catalog when
   they exit.
2. They pin the earliest ledger to the first ledger of `--start-chunk`.
3. They write `<root>/dataset.json` with these fields: schema version, tier,
   first and last chunk, first and last ledger, network passphrase, binary
   version and commit, creation time, and the `bench-ingest` flags.
4. A run into a root that has a `dataset.json` fails before it writes.
5. Remove `--catalog-dir` from `bench-ingest`. The catalog is part of the
   dataset now. The campaign runner does not use this flag.

A cold dataset and a hot dataset stay in different roots, as they do today.

### 6.3 Dataset open

`bench-read` and `bench-serve` use one function to open a dataset:
`openDataset(root)`. It does these steps in this order:

1. Read `dataset.json`.
2. Copy `<root>/catalog` to a scratch directory, and open the copy. The
   catalog is small. With the copy, the dataset root can be read-only, and
   the run cannot change it.
3. Make the registry with `query.NewRegistry`. Use the retention floor from
   the pinned earliest ledger.
4. For each ready hot chunk, open the database with the W1 function. Publish
   the handle with `PublishHandle`.
5. Set the latest ledger. Use the daemon's `lastCommittedLedger`
   (`rpcv2/progress.go`). Export or move that function. Do not copy it.
6. Call `adapters.SeedCloseTimes`, as startup does.
7. Return one release function that closes everything in reverse order.

The old cold fixture (`openColdFixture`, `freezeChunks`,
`commitDiskTxHashIndex`, `parseIndexFileName`) rebuilt the catalog from file
names. Do not port it.

Open item: the old cold fixture marked the chunk above the range as ready,
with no handle, because `NewReadView` needs a ready hot chunk. Verify this in
the first `bench-read` PR. If it is still necessary, add the key to the
scratch catalog copy, and explain it in one comment.

## 7. W4: `bench-serve`

`bench-serve` serves a benchmark dataset over JSON-RPC. It does not ingest.
Blaster is its client.

```
stellar-rpc-v2 bench-serve --dataset <root> --listen 127.0.0.1:8000 \
  [--admin-listen 127.0.0.1:8001] [--config <daemon.toml>]
```

Requirements:

1. Open the dataset with `openDataset`.
2. Make the JSON-RPC handler with the same code that the daemon uses
   (`rpcv2/jsonrpc.go`). Export what `bench-serve` needs. Do not copy the
   method table.
3. Serve these methods: `getHealth`, `getNetwork`, `getLatestLedger`,
   `getLedgers`, `getTransactions`, `getTransaction`, `getEvents`. For the
   same data, each one must return the same result as the daemon. All other
   methods return the JSON-RPC error "method not found".
4. Do not start captive core, ingestion, backfill or the lifecycle loop.
5. Use the daemon's per-method queue and duration limits. `--config` reads
   them from a daemon TOML file. Without `--config`, use the daemon defaults.
6. The dataset's close times are old. `getHealth` must report healthy
   anyway. Turn off the ledger-age check in `bench-serve` only.
7. `--admin-listen` serves `/metrics` and `/debug/pprof`, as the daemon's
   admin server does.

Acceptance:

- A test starts `bench-serve` on a test dataset and calls each served method
  over HTTP. The results agree with direct adapter calls.
- Blaster `generate` and `run` complete against `bench-serve` on a
  one-chunk dataset. Record the command lines in the PR.

## 8. W5: Storage-read metrics

### 8.1 Metric

Add one histogram:

`<namespace>_fullhistory_read_store_seconds{method, store, tier}`

| Label | Values |
|---|---|
| `method` | The JSON-RPC method of the request |
| `store` | `ledgers`, `events`, `txhash` |
| `tier` | `hot`, `cold` |

One observation is the total time that one request spent in one
(store, tier). The read view adds up the time. `ReadView.Release` records one
observation for each (store, tier) with a time above zero. Thus the metric
compares directly with `json_rpc_request_duration_seconds`.

Add a second histogram for cold reader opens:

`<namespace>_fullhistory_read_open_seconds{store}`

Use exponential buckets from 50 µs to about 10 s.

### 8.2 Timer points

| Store | Where |
|---|---|
| Ledgers, point reads | `ReadView.WithLedger` (`query/resolve.go`). `resolveLedgers` must also return the tier. |
| Ledgers, scans | The iterator inside `ScanLedgers` (`query/ledger_scan.go`). Measure only the time inside the iterator, not the caller's loop body. |
| Events | The reader that `ReadView.Events` returns, or the reads in `scanChunk` and `chunkWindow` (`query/event_page.go`). |
| Tx hash | `windowGatedIndex.Get` (hot) and `lazyColdTxIndex.Get` (cold) in `query/tx_lookup.go`. |
| Cold opens | The `OpenColdReader` calls in `query/resolve.go` and `query/tx_lookup.go`. |

`wrapAdapterRequest` gives the method name to the view when it gets the view.

### 8.3 Requirements

1. Measure the overhead with `bench-read`, warm, `ledger-scan`, before and
   after the change. The overhead must be 2% or less.
2. Add the metrics to `docs/MONITORING.md`.

## 9. W3: `bench-read`

### 9.1 Command line

```
stellar-rpc-v2 bench-read cold --cold-dir <root> [common flags]
stellar-rpc-v2 bench-read hot  --hot-dir  <root> [common flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `--cold-dir`, `--hot-dir` | required | The dataset root. The flag names are the same as in `bench-query`, to keep the runner change small. |
| `--start-chunk`, `--num-chunks` (cold), `--chunk` (hot) | the range in `dataset.json` | The chunks to read. The range must be in the dataset. |
| `--ops` | all ops | A comma-separated list of ops (Section 9.4). |
| `--requests` | 1000 | The number of requests per op. Range: 1 to 1,000,000. |
| `--concurrency` | `1` | A comma-separated list of worker counts. There is one cell for each value. |
| `--cache` | cold: `evict`; hot: `warm` | The cache mode (Section 9.2). `hot` accepts only `warm`. |
| `--seed` | 1 | The corpus seed. |
| `--corpus` | none | Read the corpus from this file. Do not make a new one. |
| `--ledgers-limit` | 10 | The ledger count for `ledger-scan`. |
| `--txpage-limit` | 200 | The transaction limit for `tx-page`. |
| `--events-limit` | 10 | The page limit for the `events-*` ops. |
| `--out` | `bench-out` | The output directory. |

The defaults for the three limits are the values in
`docs/targets.json` (`derivation`) in stellar-rpc-benchmarks.

`bench-read` gets the network passphrase from `dataset.json`. It has no
`--network-passphrase` flag.

Flag validation:

- `--cache evict` needs Linux, and every `--concurrency` value must be 1.
- `--corpus` and `--seed` together are an error.

### 9.2 Measurement model

`bench-read` is a closed loop.

- A **cell** is one op at one concurrency level W. For each cell,
  `bench-read` starts W workers. Each worker takes the next request from the
  op's request list and runs it. The cell ends when all requests are done.
- **Service time** is the time from the start of the view acquisition to the
  end of the view release. It includes the cost to open cold readers, as in
  the daemon.
- **Cell wall time** is the time from the first request start to the last
  request end. The converter calculates throughput as requests divided by
  wall time.

Cache modes:

| Mode | Before each cell | Before each request | State at the request start |
|---|---|---|---|
| `warm` | Run every request of the op one time, untimed, at concurrency W. | Nothing. | The data of the request was read one or more times before. |
| `evict` | Nothing. | Close all views. Evict every file of the dataset chunks from the OS page cache. The timer does not include the eviction. | The OS page cache holds no page of the dataset files (Section 9.6). |

### 9.3 Measured boundary

Each op calls the same adapter interfaces as the RPC handler, in the same
order and with the same arguments. It does not call the handler. It does not
encode a response.

| Op | RPC method | Calls, in order |
|---|---|---|
| `ledger-scan` | `getLedgers` | `LedgerReader.NewTx`, `GetLedgerRange`, `BatchGetLedgers(start, end)` with `end = start + limit - 1`, then close the tx as the handler does. |
| `tx-page` | `getTransactions` | `LedgerReader.NewTx`, `GetLedgerRange`, then `WithLedgerRaw` for each ledger until the page holds `--txpage-limit` transactions. In each ledger, build the views with `ingest.LedgerTransactionViewRange`, as the handler does (`methods/get_transactions.go`). Do not call `store.ParseTransactionView`: it builds the response. |
| `tx-hash-hit` | `getTransaction` | `TransactionReader.GetTransaction(hash)`, then `GetLedgerRange`. |
| `tx-hash-miss` | `getTransaction` | The same calls, for a hash that is not in the dataset. |
| `events-*` | `getEvents` | `ReadView.QueryEventsFrom(cursor, limit)`, the call that `eventsapi/get_events_v1.go` makes. |

The PR that adds an op must show the handler's calls next to the op's calls,
with file and line.

Each op checks its result:

- `tx-hash-hit` must find the hash. `tx-hash-miss` must get
  `store.ErrNoTransaction`.
- `ledger-scan` must return `--ledgers-limit` ledgers, or all ledgers to the
  end of the range.
- `events-*` must return only events that match the filter.

A failed check is a request error (Section 9.8).

### 9.4 Ops and corpus

The **corpus** is the request list of each op. `bench-read` makes it from
`--seed` and the dataset before the first cell.

| Op | How to make each request |
|---|---|
| `ledger-scan` | Start ledger: uniform in `[first, last - limit + 1]`. |
| `tx-page` | Start ledger: uniform in `[first, last]`. |
| `tx-hash-hit` | Select a ledger uniformly. Read it. Select one of its transactions uniformly. Skip empty ledgers and hashes that are already in the list. Fail after `20 × --requests` draws. |
| `tx-hash-miss` | 32 random bytes. |
| `events-all` | No filter. |
| `events-busy` | A contract, uniform from the 10 contracts with the most events. |
| `events-quiet` | A contract, uniform from the 10% of contracts with the fewest events (1 or more). |
| `events-topic` | A (contract, first topic) pair, uniform from the 10 most frequent pairs. |
| `events-absent` | A random 32-byte contract ID that is not in the term scan. The query reads the full window and finds nothing. |

For all `events-*` ops: the start ledger is uniform in the range, the
direction is ascending, and the limit is `--events-limit`.

The term scan reads events from up to 32 chunks, spread across the range, and
up to 5,000 events per chunk. It counts contract IDs and
(contract, first topic) pairs. Reuse `eventTerms` from #13.

Rules:

1. Each op has its own random source: PCG, seeded from (`--seed`, op index).
   There is no random source per request.
2. If an op has no candidates (for example, a dataset with no events), the
   corpus step fails. The message names the op, so the user can remove it
   from `--ops`.
3. `bench-read` writes the corpus to `<out>/corpus.json`. The file holds the
   schema version, the SHA-256 of `dataset.json`, the seed, and each op's
   request list.
4. With `--corpus <file>`, `bench-read` reads the list and makes no new one.
   If the dataset hash is different, the run fails.
5. The corpus step is not a cell. Its wall time goes to the `corpus` setup
   row.

Do not port the tx-hash sampler from #13 (per-chunk targets, 16 hashes per
ledger, draw budgets). One hash per request from a uniform ledger is simpler,
and it reads the range at random.

### 9.5 Output

The `--out` directory holds these files:

| File | Content |
|---|---|
| `<op>.csv` | One row `total_c<W>` for each concurrency level: the service time. `n_items` is the sum of the items that the requests returned. |
| `driver.csv` | One row `<op>_c<W>` for each cell: the cell wall time. `n_items` is the request count. The setup rows are `open`, `corpus`, `evict` (the time per eviction pass) and `peak_rss_bytes`. |
| `corpus.json` | The corpus (Section 9.4). |
| `invocation.json` | The existing record, plus `status` (`running`, `ok` or `failed`). The extra fields are: tier, cache mode, concurrency list, dataset hash, corpus hash and `maxResidentAfterEvict`. |

Rules:

1. The CSV columns are the existing ones:
   `stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns`.
2. The `_ns` columns hold durations. The only exception is
   `peak_rss_bytes`, which exists today. No other unit goes in these
   columns.
3. A cell goes into the CSV only if it completed.
4. Write `invocation.json` through a temporary file and a rename. Reuse this
   code from #12.

This format matches the converter's closed-loop path (`build_queries`). R1
changes the runner to one output directory per run, with all ops in it.

### 9.6 Page-cache eviction and verification

1. Eviction: call `POSIX_FADV_DONTNEED` on every file of the dataset chunks.
   Reuse `evict_linux.go` from #13.
2. Verification: after the first eviction, and after every 100th eviction,
   map each file and count its resident pages with `mincore`. Record the
   highest resident fraction in `invocation.json`
   (`maxResidentAfterEvict`).
3. If the resident fraction is more than 1%, the run fails.
4. In-process caches: at concurrency 1, no cold reader stays open after a
   request (Section 2.4). The events cold reader loads `index.hash` into
   memory each time it opens. The daemon does the same, so this cost is part
   of the request.

### 9.7 Hot tier

- `bench-read hot` opens the chunk database with the W1 function. It does not
  change the dataset.
- The hot tier supports only `warm`. RocksDB keeps a block cache in the
  process, and `bench-read` cannot clear it before each request.
- The events warmup runs when the database opens. Its time goes to the `open`
  setup row.

### 9.8 Errors and cancel

1. A request error stops the run. `bench-read` stops the other workers,
   writes the completed cells, and sets `status` to `failed` with the error.
2. SIGINT does the same, with the error "canceled".
3. There are no partial cells and no PARTIAL marks.

A static dataset does not cause errors from load. Thus an error is a
defect, and the run must stop.

### 9.9 Package layout

All files go in `cmd/stellar-rpc/internal/rpcv2/bench`.

| File | Content |
|---|---|
| `read.go` | Command tree and flags. |
| `read_ops.go` | The op table: name, RPC method, corpus function, run function. |
| `read_corpus.go` | Corpus generation and `corpus.json`. |
| `read_run.go` | The closed-loop cell runner. |
| `read_cache.go`, `evict_linux.go`, `evict_other.go` | Eviction and `mincore`. |
| `dataset.go` | `openDataset`, shared with `bench-serve`. |
| `serve.go` | `bench-serve`. |
| `README.md` | How to run, what the output holds, and one list of limits. |

`--ops` parses against the op table. Do not use a `switch` with an
unreachable `default`.

## 10. Campaign changes (stellar-rpc-benchmarks)

### 10.1 R1: `bench-read` in the campaign

1. `runner/internal/plan/plan.go`: replace the `bench-query` legs with
   `bench-read` legs. Use one leg per (tier, dataset, chunk, run), with all
   ops. Use the directory layout `query-<tier>-<unit>-run<R>`.
2. `converter/convert.py`: read the per-op files through the closed-loop
   path. Do not attach SLA verdicts to `bench-read` results.
3. Viewer (`docs/app.js`, `docs/summary.js`): show the op names.
4. Keep the paced (`_r<rate>`) path until R2 replaces the SLA verdicts.

### 10.2 R2: Blaster legs and SLA verdicts

1. Add a leg that starts `bench-serve` on the dataset of an ingest leg and
   runs Blaster against it.
2. Use Blaster's concurrent mode. Use the mix from `docs/targets.json`
   (txhash 0.60, events 0.20, txpage 0.15, ledgers 0.05). Use steps at the
   Light, Standard and Heavy rates (250, 500 and 1000 rps).
3. Read `/metrics` from `bench-serve` before and after the leg. Report the
   storage share of each method from the W5 histograms.
4. The converter reads the Blaster results JSON. It attaches the SLA verdict
   per method from the p99 at the Standard step. The tier is the dataset's
   tier: a cold dataset gives the cold verdict, and a hot dataset gives the
   hot verdict.
5. At the verdict step, make sure that Blaster did not saturate: no timeouts,
   and no worker-cap limit. If it did, add scheduled-time latency to Blaster
   before the verdicts are used.

## 11. Code rules

The old stack has these defects. Each rule gives an example from #12, #13 or
#14.

| # | Rule | Example of the defect |
|---|---|---|
| 1 | Each PR is a vertical slice. It runs end to end, and it contains its own tests. | #12 adds a dispatcher that nothing calls until #13. The tests for #13 are in #14. |
| 2 | Doc comments say what the code does and what its contract is. Do not describe the callers, the history, or the options you rejected. Most comments are 5 lines or fewer. | `querySpecs` has a 35-line comment. |
| 3 | Validate the input one time, at the flag boundary. The inner code trusts the plan. | `parseTargetRPS` and `runPacedLeg` both check the rate floor. `plan()` and `buildTxHashCorpus` both check the corpus size. |
| 4 | No side channels. Functions return values. The command makes the invocation metadata. | `newQueryRequest` writes to `p.Extra[...]` after a nil check. |
| 5 | Use one cleanup path: `defer`, or one release stack. | `openHotFixture` repeats `_ = db.Close(); releaseCat()` in four error branches. |
| 6 | Put one unit in each column. | Rates as milli-rps in `_ns` columns. |
| 7 | Use data, not code paths: the corpus is a list, and the ops are a table. | `legRNG` mixing constants; `default: // Unreachable`. |
| 8 | Do not add limits or overflow checks for cases that the flag ranges already prevent. | The `maxLegRequests` memory notes; the `Duration` overflow check. |
| 9 | Use one term for each concept (Section 13). | "leg" and "rate run"; "shed" and "dropped". |
| 10 | The README is one page or less: what it measures, how to run it, what the output holds, and one list of limits. | The `bench-query` README is mostly caveats. |
| 11 | Wrap an error one time, with the operation and the key values. Do not put advice paragraphs in error strings. | Multi-sentence error strings in `commitDiskTxHashIndex`. |
| 12 | Lint must be clean. A new `nolint` needs a one-line reason. | — |
| 13 | Keep each PR to about 600 lines of non-test Go or fewer. | #13 adds 2,552 lines. |

Before review, run a cleanup pass on the diff for comments and defensive code.

## 12. Test plan

| Area | Test |
|---|---|
| Flags | Table test: `evict` needs Linux and concurrency 1; `hot` rejects `evict`; an unknown op fails; `--corpus` with `--seed` fails. |
| Corpus | The same seed and dataset give a byte-identical `corpus.json`. `--corpus` reads it back. A different dataset hash fails. |
| Runner | Use a fake op. Each cell runs exactly `--requests` requests. The highest number of concurrent requests is W. The first error stops the run, and no later cell is written. SIGINT writes only the completed cells. |
| Ops | Use a small dataset made with the `bench-ingest` code. Reuse the `rpcv2test` helpers from #14 (ledger packs with multi-transaction ledgers). Each op returns the expected items. `tx-hash-hit` finds every hash. `tx-hash-miss` finds none. Each `events-*` op returns only matching events. |
| No dataset change | For cold and hot: record the file set (name, size, modification time) before and after a run. The two sets must be equal. |
| Eviction | Linux only. Read a test file, evict it, and check that `mincore` shows 1% or less resident. |
| CSV | A golden file for row names and columns. R1 adds a converter test. |
| W5 metrics | One request records one observation per (store, tier), with the correct method. Check the overhead (Section 8.3). |
| `bench-serve` | See Section 7. |

Commands for each PR:

```
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/... \
  ./cmd/stellar-rpc/internal/rpcv2/query/... \
  ./cmd/stellar-rpc/internal/rpcv2/stores/hotchunk/...
make go-check-branch BASE=feature/full-history
```

## 13. Terms

| Term | Meaning |
|---|---|
| Dataset | A root directory that `bench-ingest` wrote: the artifacts or hot databases, the catalog, and `dataset.json`. |
| Tier | Where a chunk is served from: `hot` (chunk database) or `cold` (frozen artifacts). |
| Op | One kind of storage read, for example `tx-hash-hit`. |
| Request | One call of an op with one set of parameters. |
| Corpus | The request list of each op. |
| Cell | One op at one concurrency level. |
| Service time | The time of one request, from view acquisition to view release. |
| Cache mode | `warm` or `evict` (Section 9.2). |

## 14. Delivery plan

### 14.1 stellar-rpc

| PR | Content | Acceptance |
|---|---|---|
| P0 | This spec. | Review by the team. |
| P1 | W1: read-only hot open with an events facade. | Section 6.1. |
| P2 | W2: durable dataset catalog and `dataset.json`. | A cold run and a hot run each leave a catalog and a `dataset.json`. A second run into the same root fails. |
| P3 | `bench-read` core: `openDataset`, the cell runner, `ledger-scan`, the CSV output, `warm` mode, the README, and registration in the binary. | `bench-read cold` and `bench-read hot` run on test datasets. The runner, "no dataset change" and CSV tests pass. |
| P4 | `tx-hash-hit`, `tx-hash-miss`, `tx-page`, and `corpus.json` with `--corpus`. | The ops and corpus tests pass. |
| P5 | The `events-*` ops and the term scan. | The events op tests pass. |
| P6 | `evict` mode with `mincore` verification. | The eviction test passes on Linux. |
| P7 | W5: storage-read metrics. It can start after P0. | Section 8.3. |
| P8 | W4: `bench-serve`. | Section 7. |

### 14.2 stellar-rpc-benchmarks

| Change | Starts after |
|---|---|
| R1 | P3 to P6 |
| R2 | P7 and P8 |

### 14.3 The old stack

1. Do not merge #12, #13 or #14.
2. Keep their branches until R2 gives the SLA verdicts (Q1).
3. Reuse these parts, rewritten to the rules in Section 11:
   - the `invocation.json` atomic write and status (#12, `invocation.go`);
   - `newSchemaCSVSink` (#12, `csvsink.go`);
   - `evict_linux.go` and `evict_other.go` (#13);
   - `eventTerms` (#13, `query_corpus.go`);
   - the ledger-pack helpers in `rpcv2test` (#14).
4. Discard these parts: `query_dispatch.go`, the `query-accounting.csv`
   schema, the driver rate rows, the cold catalog rebuild, the tx-hash
   sampler budgets, and `legRNG`.

## 15. Open questions

| # | Question | Recommendation |
|---|---|---|
| Q1 | Between R1 and R2, which tool gives the SLA verdicts? | The campaign keeps the `bench-query` branch for its query legs until R2 ships. |
| Q2 | Is 1% the correct limit for the resident fraction after eviction? | Start at 1%. Change it after the first campaign data. |
| Q3 | Do the W5 metrics go to production, or only to `bench-serve`? | Production. The label count is small (4 methods × 3 stores × 2 tiers). Confirm the metric names with the team. |
| Q4 | Do we need multi-chunk hot datasets? | No, not in this work. |
| Q5 | Does `NewReadView` still need a ready hot chunk above a cold range? | Verify in P3 (Section 6.3). |
