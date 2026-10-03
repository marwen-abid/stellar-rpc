# Storage Benchmarks

This package has two benchmark commands:

- `bench-ingest` writes a storage dataset and measures ingestion.
- `bench-query` reads that dataset and measures read latency.

`bench-query` has one subcommand for each storage tier:

- `bench-query cold` reads frozen artifacts.
- `bench-query hot` reads a hot chunk database.

The tier names do not describe the state of the OS page cache.

This document defines the terms and the formulas of `bench-query`. It also
defines `run.json`, which both commands write.

## 1. Method

### 1.1 Open-loop load generator

`bench-query` is an **open-loop load generator**. An open-loop generator
starts each request at a time that a schedule sets. It does not wait for a
response before it starts the next request.

A **closed-loop** generator starts a request only after the previous response
arrives. When the store is slow, it sends fewer requests, and the results do
not show the delay. This error is **coordinated omission**.

Reference: B. Schroeder, A. Wierman, M. Harchol-Balter, "Open Versus Closed:
A Cautionary Tale", NSDI 2006.

### 1.2 Constant arrival rate

The schedule has a **constant arrival rate**: the time between two due times
is always the same. The k6 load tool uses the same model in its
[`constant-arrival-rate`](https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/constant-arrival-rate/)
executor.

| k6 term | `bench-query` term |
|---|---|
| scenario | scenario |
| iteration | iteration |
| `rate` / `timeUnit` | `--target-rps` |
| `duration` | `--duration` |
| `maxVUs` | `maxConcurrent` (512) |
| `dropped_iterations` | `dropped` |

### 1.3 Bounded concurrency and dropped iterations

At most 512 requests (`maxConcurrent`) run at the same time. If an iteration
becomes due while 512 requests run, the generator does not start it. The
generator **drops** the iteration and counts it. The general name for this
method is load shedding. It prevents an unbounded queue and an unbounded
number of goroutines in the client.

## 2. Terms

| Term | Definition |
|---|---|
| scenario | One query type at one target rate for one duration. A run does one or more scenarios, one after the other. |
| iteration | One scheduled request in a scenario. |
| due time | The time at which an iteration must start. |
| reach | The generator reaches an iteration when its wait for the due time of that iteration ends. If you do not cancel the run, the generator reaches all iterations. |
| warmup iteration | An iteration before the measured iterations. The generator starts it at the same rate, but does not record its timings. |
| measured iteration | An iteration after the warmup iterations. The report includes it. |
| planned | The number of measured iterations that the scenario reached. |
| started | The number of measured iterations whose request the generator started. |
| dropped | The number of measured iterations that the generator did not start, because 512 requests ran. |
| succeeded | The number of started requests that returned no error. |
| failed | The number of started requests that returned an error. |
| response | The end of a started request. A failed request also has a response. |
| schedule | The due times of the measured iterations. In the formulas, `schedule` is the length of the schedule: `planned × interval`. |
| start delay | The time from the due time of an iteration to the time the generator starts it. |
| latency | The time from the start of the request body to its end. **This is the main result.** |
| latency from due | The time from the due time of an iteration to its response. |
| elapsed | The larger of two values: the schedule length, and the time from the first measured due time to the last measured response. |
| overrun | The time from the end of the schedule to the last measured response. It is 0 if the last response came before the end of the schedule. |

## 3. Schedule

Symbols:

- `r`: the target rate, in requests per second (`--target-rps`).
- `D`: the duration (`--duration`).
- `W`: the number of warmup iterations (`--warmup`).
- `t0`: the clock time when the scenario starts.

Formulas:

```
interval = round(1 s / r)                       (nanoseconds)
planned  = round(r × D)                         (warmup not included)
due(i)   = t0 + i × interval                    for i = 0 … W + planned − 1
```

- Iterations `0` to `W − 1` are warmup iterations.
- The measured iterations start at `i = W`.
- Each due time is absolute. A late start does not move the due times that
  follow it.

Limits:

- `r` must be a positive finite number.
- `r` must be 1,000,000,000 or less, so that `interval` is 1 ns or more.
- `interval` must fit in a Go `time.Duration` (about 292 years).
- `planned` must be 1 or more.
- `W + planned` must be 100,000,000 (`maxIterations`) or less.
- A negative `W` counts as 0.

If you cancel a run (SIGINT or SIGTERM), the generator stops its wait at once
and starts no more iterations. It then waits for the running requests to
return. The cancel also stops these requests, and they count as failed.
`planned` is then the number of measured iterations that the generator
reached.

## 4. Request timings

For each measured iteration `i`:

```
start_delay(i)      = max(start(i) − due(i), 0)
latency(i)          = body_end(i) − body_start(i)
latency_from_due(i) = response(i) − due(i)
```

- `start(i)` is the clock time when the generator reaches iteration `i`.
- `body_start(i)` and `body_end(i)` are the clock times when the request body
  starts and ends. The request body includes the read-view acquisition.
- `response(i)` is the clock time when the request goroutine reads the clock,
  after the request body returns.
- `latency_from_due(i)` is almost equal to `start_delay(i)`, plus the time
  that the new goroutine waits for a CPU, plus `latency(i)`.

Which timings each distribution contains:

| Distribution | Contains |
|---|---|
| `latency` | Succeeded requests only. |
| `latency_from_due` | Succeeded requests only. |
| `start_delay` | All measured iterations, dropped iterations included. |

**Read `latency` as the result.** It is the time that one request takes, from
the start of the request body to its end. `latency_from_due` adds client-side
delay. Read it together with
`start_delay`. Failed and dropped requests are not in `latency`. Always read
`latency` together with the counts in `scenarios.csv`.

## 5. Counts, windows and rates

```
planned        = started + dropped
started        = succeeded + failed
schedule       = planned × interval
elapsed        = max(schedule, last_response − due(W))
overrun        = elapsed − schedule
achieved_rps   = started / schedule
completion_rps = succeeded / elapsed
process_cpu    = cpu(end) − cpu(start(W))
```

- `last_response` is the last response of a measured iteration. A failed
  request counts. If no measured request has a response, `elapsed` is equal
  to `schedule`.
- `achieved_rps` is the rate at which requests started.
- `completion_rps` is the rate of successful responses over the full elapsed
  time.
- `cpu(t)` is the user plus system CPU time of the whole process at time `t`.
  `end` is the time after the last request of the scenario returns. The
  process includes the store and the generator, so `process_cpu` includes
  both.

Signs that the store or the client is overloaded:

- `dropped` is more than 0.
- `overrun` increases from scenario to scenario.
- The `start_delay` percentiles increase.

## 6. Percentiles

For one distribution, sort the `n` samples in ascending order:
`s[0] ≤ s[1] ≤ … ≤ s[n − 1]`.

```
pq    = s[min(floor(q × n), n − 1)]             for q = 0.50, 0.90, 0.99
max   = s[n − 1]
total = s[0] + s[1] + … + s[n − 1]
count = n
```

- Zero durations stay in the distribution.
- When `n` is 100 or less, `p99` is equal to `max`.
- The log shows a warning for each `latency` row that has fewer than 100
  samples.

## 7. Output files

`bench-query` always writes `run.json` to `--out`. It writes `latency.csv` and
`scenarios.csv` when at least one scenario has a result. If `--out` already
contains a `.csv` file, the command stops before it changes `--out`. This
prevents a mix of results from two runs.

If a measured request fails, the run fails after the command adds its scenario
to the report. If the run fails or you cancel it, the command writes the
scenarios that have a result, and the log marks the report as PARTIAL. A
scenario that you cancel before its first measured iteration has no rows.

### 7.1 `latency.csv`

One row for each distribution of each scenario. A distribution with no
samples has no row.

| Column | Definition |
|---|---|
| `query_type` | The query type: `ledgers`, `txpage`, `txhash` or `events`. |
| `target_rps` | The target rate `r` of the scenario. |
| `metric` | `latency`, `latency_from_due` or `start_delay`. See section 4. |
| `outcome` | `all`, or for `txhash` also `found` and `not_found`. `start_delay` is always `all`. |
| `count` | `n`, the number of samples. |
| `items` | The sum of the items that the responses carried. `0` for `start_delay`. |
| `total_ns` | The sum of the samples, in nanoseconds. |
| `p50_ns`, `p90_ns`, `p99_ns` | Percentiles, in nanoseconds. See section 6. |
| `max_ns` | The largest sample, in nanoseconds. |

### 7.2 `scenarios.csv`

One row for each scenario, in run order.

| Column | Definition |
|---|---|
| `query_type` | The query type. |
| `target_rps` | The target rate `r`. |
| `planned`, `started`, `dropped`, `succeeded`, `failed` | The counts of the measured iterations. See section 5. |
| `warmup_planned` | The number of warmup iterations that the scenario reached. |
| `warmup_dropped`, `warmup_failed` | The dropped and failed warmup iterations. |
| `achieved_rps` | `started / schedule`. |
| `completion_rps` | `succeeded / elapsed`. |
| `schedule_ns`, `elapsed_ns`, `overrun_ns` | The time windows, in nanoseconds. |
| `process_cpu_ns` | The process CPU time of the measured phase, in nanoseconds. |
| `page_cache_evict_ns` | The time of the page-cache eviction before the scenario. Empty when no eviction occurred. See section 10. |

### 7.3 `run.json`

The command writes `run.json` before the run starts, with `status` set to
`running`. It writes the file again when the run ends, with `status` set to
`ok` or `failed`. Each write replaces the file in one step. If `status` is
`running` after the process stopped, the run stopped before its last write.

| Key | Definition |
|---|---|
| `schemaVersion` | The version of this format. The current version is 2. |
| `command` | The command path, for example `stellar-rpc-v2 bench-query cold`. |
| `flags` | The value of each flag, default values included. |
| `binary` | The version, commit, build time and branch of the binary. |
| `hostname` | The host name. |
| `gomaxprocs` | The value of `GOMAXPROCS` for the process. |
| `numCpu` | The number of logical CPUs that the process can use. |
| `startedAt`, `finishedAt` | UTC start and end times (RFC 3339). `finishedAt` is absent while the run continues. |
| `peakRssBytes` | The peak resident set size of the process (`VmHWM`). Absent while `status` is `running`, and on systems without `/proc`. |
| `settings` | Values that the run sets and that the flags do not show. Absent while `status` is `running`, and when empty. |
| `setupNs` | One-time setup durations, in nanoseconds, for example the store open. Absent while `status` is `running`, and when empty. |
| `status` | `running`, `ok` or `failed`. |
| `error` | The error message of a failed run. Absent when the run succeeded. |

`bench-query` writes these keys in `settings` and `setupNs`:

| Key | Definition |
|---|---|
| `settings.cacheScenario` | `cold-start`, `warm-run` or `existing-cache`. See section 10. |
| `settings.pageCacheEviction` | `off`, `requested` or `unsupported-on-this-platform`. See section 10. |
| `settings.txhashPoolHashes` | The number of hashes in the transaction pool. See section 9.1. |
| `settings.txhashPoolLedgers` | The number of ledgers that supplied a hash to the pool. |
| `settings.eventsPool` | `derived` or `unfiltered`. See section 9.2. |
| `settings.fixedReadRange` | The types whose span covers all ledgers of the dataset, for example `ledgers,txpage`. Absent when no span covers them. |
| `setupNs.storeOpen` | The time to open the dataset, before the first scenario. |

## 8. Datasets

### 8.1 Scratch catalog

Each `bench-query` run creates a scratch catalog in a `bench-query-catalog-*`
directory under the dataset root (`--cold-dir` or `--hot-dir`). Use
`--catalog-dir` to put the catalog in a different directory. A read-only
dataset root needs this flag. The run removes the directory when it ends. If
the run stops before it ends, the directory stays. Remove it before you
measure again.

### 8.2 Hot tier database

`bench-query hot` opens the chunk database read-write, as the daemon opens a
resumed chunk. The open replays and flushes the write-ahead log, and it can
start compaction. Thus, after a run, the state on the disk is different from
the state that ingest left. A second run on the same `--hot-dir` measures a
compacted database. The open takes an exclusive lock, so the run cannot use
the directory of a running daemon.

## 9. Query types

All requests use the storage read paths through `query.ReadView`. The
measurement does not include RPC handlers, response serialization or network
work.

| Type | Request | `items` |
|---|---|---|
| `ledgers` | One `ReadView.ScanLedgers` over `--ledgers-span` ledgers, from a random start ledger. This is the read of `getLedgers`. | Ledgers. |
| `txpage` | A scan of `--txpage-span` ledgers that makes the transaction views, envelopes included, up to `--txpage-limit` transactions. This is the read of `getTransactions`. It does not make or serialize the full RPC response. | Transactions. |
| `txhash` | One by-hash lookup of a hash from the transaction pool, through the tx-hash indexes. This is the read of `getTransaction`. | 1 if found, 0 if not found. |
| `events` | One page of `--events-limit` events or fewer, from a random start ledger to the last ledger of the dataset, with a filter set from the events pool. This is the read of `getEvents`. | Events. |

If `--ledgers-span` or `--txpage-span` is equal to or more than the number of
ledgers in the dataset, each request of that type reads the same ledgers. The
log then shows a warning, and `settings.fixedReadRange` lists those types.

An `events` request scans one page window of 10,000 ledgers or fewer. If the
filter matches no event in that window, the request returns an empty page. The
request succeeds with zero items. Before you read the `events` percentiles as
filtered-read latency, examine `items` in `latency.csv`.

### 9.1 Transaction pool

The transaction pool holds the hashes that `txhash` looks up. The run builds
it before the first `txhash` scenario.

- `--txhash-pool-size` sets the maximum number of hashes. The default is 512.
  The value must be 1 to 1,000,000.
- The sampler reads random ledgers, and takes 16 hashes or fewer from each
  ledger. Each chunk supplies its share of the pool.
- `--seed` sets the draws, so the same seed and dataset give the same pool.
- The pool can be smaller than the maximum. The log then shows a warning. A
  pool with no hashes is an error.

`settings.txhashPoolHashes` gives the pool size, and
`settings.txhashPoolLedgers` gives the number of ledgers that supplied a hash.

`--not-found-fraction` sets the fraction of `txhash` lookups that ask for a
hash that is not in the dataset. The default is 0.12. The value must be 0 to 1.
Such a hash is 32 random bytes. A not-found lookup probes every hot index and
then every cold window index, so it is the slowest lookup. `latency.csv` shows
these lookups with the outcome `not_found`.

### 9.2 Events pool

The events pool holds the filter sets that `events` uses. The run scans up to
20,000 stored events of the dataset to make it. The pool holds the unfiltered
read, the contracts with the most events, and the most frequent pair of
contract and first topic. `settings.eventsPool` is `derived` when the scan
found a filter term, and `unfiltered` when the pool holds only the unfiltered
read.

## 10. Cache controls

Both tiers accept `--warmup`. The default is 0 for `cold` and 20 for `hot`.

`bench-query cold` also accepts `--evict-page-cache`. The default is true. On
Linux, it requests OS page-cache eviction of the dataset files before each
scenario. The eviction occurs before the warmup. The pool build occurs before
both, and it can fill the caches. `bench-query hot` does not request
eviction.

`settings.cacheScenario` records the requested controls:

- `cold-start`: eviction is requested and there is no warmup. This value also
  applies on a platform that cannot evict.
- `warm-run`: the warmup is more than 0, with or without eviction.
- `existing-cache`: there is no eviction and no warmup.

`settings.pageCacheEviction` is `off`, `requested` (Linux) or
`unsupported-on-this-platform`.

## 11. Known limits

### 11.1 Timer precision

The generator waits for each due time on a Go timer. A Go timer can end its
wait up to about 1 ms late. In our measurements, the delay was about 0.5 ms at
p50 and about 1.1 ms at p99.

- This delay adds to `start_delay` and `latency_from_due`.
- This delay does not change `latency`.

### 11.2 Bursts above about 1,000 rps

Above about 1,000 rps, the interval is shorter than the timer delay. After a
late wait, the generator starts all overdue iterations together. The average
rate stays correct, but the requests arrive in small bursts. At 10,000 rps, a
burst has about 5 to 10 requests.

### 11.3 Shared process

The generator and the store run in the same process. They use the same CPUs
and the same Go scheduler. `process_cpu_ns`, `gomaxprocs` and `numCpu` show
how much CPU the run used and had.
