# Storage Benchmarks

`bench-ingest` prepares storage datasets. `bench-query cold` reads frozen
artifacts; `bench-query hot` reads a hot chunk database. These tier names do
not describe the OS page-cache state.

Each `bench-query` run creates a scratch catalog under the dataset root. Set
`--catalog-dir` to put that catalog elsewhere. A read-only dataset root needs
this flag. A run that stops before it finishes leaves its
`bench-query-catalog-*` directory. Remove that directory before you measure
again.

## Query Measurements

Queries use storage read paths through `query.ReadView`. They exclude RPC
handlers, response serialization and network work. `txpage` constructs
transaction views up to its page cap. It does not build or serialize the full
RPC response. Results do not establish endpoint SLAs.

Each rate leg schedules `round(rps × duration)` measured positions. A rate and
duration that round to zero measured positions fail the run.
Warmup positions are unmeasured and can overlap measured work. A full in-flight
limit sheds a position. Scheduled latency spans due time to successful
completion. Service latency covers the request body. It includes read-view
acquisition. Failed and shed requests do not enter latency percentiles. A
request failure fails the run. The completed leg's counters and successful
samples stay in the partial CSV report. Cancellation discards the interrupted
leg.

`query-accounting.csv` reports scheduled, dispatched, successful, failed and shed counts
in `n_items`. Scheduled equals dispatched plus shed. Dispatched equals
successful plus failed. Arrival is the measured position count times the pacing
interval. Elapsed is `max(arrival, wall)`. Drain is `max(wall - arrival, 0)`. Completion
throughput is successful requests divided by elapsed seconds. This file also
reports the target rate. All rate rows
encode requests per second times 1000 in the duration columns, not nanoseconds.

`driver.csv` keeps the legacy wall, `_millirps`, `_lag` and `_shed` rows with
their existing formats and semantics. The results converter explicitly reads
the query-type CSVs and `driver.csv`; it misclassifies unknown driver rows as
setup. The separate accounting file preserves compatibility with that converter.
The `_shed` row is duplicated in the accounting file to keep all counts together.
The `evict` row takes one observation per (type, rate) leg. Its `n` is thus the
leg count, and its `total_ns` is the summed eviction time. Its percentiles are
the per-leg eviction cost.

The legacy `_millirps` row reports successful requests divided by arrival seconds,
not completion throughput. Its `n_items` retains the successful count. The wall
row's `n_items` does the same. Rate and window rows in `query-accounting.csv` use
`n_items=0`. Zero counts, rates, lag and drain are
retained. Logs warn on shedding and fewer than 100 successful samples. A `txhash`
found/miss subgroup warns only when the rate, duration and miss fraction predicted
at least 100 samples for it. This is a basic warning, not a statistical gate.

An `events` request scans at most one page window of 10,000 ledgers. A filter
that matches nothing in that window returns an empty page. The request counts as
successful with zero items. Check `n_items` in `events.csv` before you read the
`events` percentiles as filtered-read latency.

## Cache Controls

Both tiers accept `--warmup`: cold defaults to 0 and hot to 20. Cold also accepts
`--evict-page-cache`, default true. This requests OS page-cache eviction before
each leg on Linux. It does not guarantee cold caches or reset process caches.
Eviction precedes warmup. Corpus preparation runs before both and can populate
caches. No eviction is requested for the hot tier.

Invocation `extra.cacheScenario` records control intent:

- `cold-start`: eviction requested and no warmup, even on unsupported platforms.
- `warm-run`: warmup is positive, with or without prior eviction. The warmup count
  does not prove steady state. Warmup positions can also be shed or fail.
- `existing-cache`: neither eviction nor warmup requested.

`extra.pageCacheEviction` is `off`, `requested` on supported platforms, or
`unsupported-on-this-platform`. Neither the scenario nor this field proves that
the data was cold when measurement began.

## Hot Tier Database

`bench-query hot` opens the chunk database read-write. This is the daemon's own
resumed-chunk open. The open replays and flushes the write-ahead log. It also
permits compaction. Thus the on-disk state after a run differs from the state
ingest left. A second run over the same `--hot-dir` measures a compacted
database. The open takes an exclusive lock, so it cannot share a directory with
a running daemon. A read-only open composes no events facade, so it cannot serve
the `events` type. See `hotchunk.OpenReadOnly`, and #772 for a warmed read-only
variant.

## Transaction Corpus

`--txhash-corpus-size` defaults to 512 and accepts 1 through 1,000,000. It is a
best-effort cap. Sampling uses seeded random ledger draws with proportional
chunk targets. It takes at most 16 hashes per ledger. It trims each draw to the
remaining target. The range can hold more chunks than the requested corpus size.
Then only about `--txhash-corpus-size` chunks contribute a hash. The pool thus
samples the range. It does not cover the range.
Each chunk's draw budget is at least 512 or 16 times the ledgers needed at 16
hashes each. Empty data and repeated draws
can leave the pool underfilled; this produces a warning. Zero hashes is an error.

Invocation extras `txhashCorpusHashes` and `txhashCorpusLedgers` record actual
pool size and contributing ledger count. The requested size is in invocation
flags. The same seed and dataset reproduce the sampling. A finite pool can cause
cache reuse. It is not a model of the full production working set.
