# Storage Benchmarks

`bench-ingest` prepares storage datasets. `bench-query cold` reads frozen
artifacts; `bench-query hot` reads a hot chunk database. These tier names do
not describe the OS page-cache state.

## Query Measurements

Queries use storage read paths through `query.ReadView`. They exclude RPC
handlers, response serialization and network work. In particular, `txpage`
constructs transaction views up to its page cap; it does not build or serialize
the full RPC response. Results do not establish endpoint SLAs.

Each rate leg schedules `max(1, round(rps * duration))` measured positions.
Warmup positions are unmeasured and can overlap measured work. A full in-flight
limit sheds a position. Scheduled latency spans due time to successful completion;
service latency covers the request body, including read-view acquisition.
Failed and shed requests do not enter latency percentiles. A request failure
fails the run, but its completed leg's counters and successful samples remain in
the partial CSV report. Cancellation discards the interrupted leg.

`driver.csv` reports scheduled, dispatched, successful, failed and shed counts
in `n_items`. Scheduled equals dispatched plus shed; dispatched equals successful
plus failed. Arrival is the measured position count times the pacing interval.
Elapsed is `max(arrival, wall)`; drain is `max(wall - arrival, 0)`. Completion
throughput is successful requests divided by elapsed seconds. All rate rows
encode requests per second times 1000 in the duration columns, not nanoseconds.

The legacy `_millirps` row remains for the results converter. It reports
successful requests divided by arrival seconds, not completion throughput.
Its `n_items`, and the legacy wall row's `n_items`, retain the successful count.
New rate and window rows use `n_items=0`. Zero counts, rates, lag and drain are
retained. Logs warn on shedding and fewer than 100 successful samples, including
each `txhash` found/miss subgroup. This is a basic warning, not a statistical gate.

## Cache Controls

Both tiers accept `--warmup`: cold defaults to 0 and hot to 20. Cold also accepts
`--evict-page-cache`, default true. This requests OS page-cache eviction before
each leg on Linux. It does not guarantee cold caches or reset process caches.
Eviction precedes warmup. Corpus preparation runs before both and can populate
caches. No eviction is requested for the hot tier.

Invocation `extra.cacheScenario` records control intent:

- `cold-start`: eviction requested and no warmup, even on unsupported platforms.
- `warm-run`: warmup is positive, with or without prior eviction. The warmup count
  does not prove steady state; warmup positions can also be shed or fail.
- `existing-cache`: neither eviction nor warmup requested.

`extra.pageCacheEviction` is `off`, `requested` on supported platforms, or
`unsupported-on-this-platform`. Neither the scenario nor this field proves that
the data was cold when measurement began.

## Transaction Corpus

`--txhash-corpus-size` defaults to 512 and accepts 1 through 1,000,000. It is a
best-effort cap. Sampling uses seeded random ledger draws with proportional
chunk targets, at most 16 hashes per ledger, and trims to the remaining target.
Each chunk's draw budget is at least 512 or 16 times the ledgers needed at 16
hashes each. Empty data and repeated draws
can leave the pool underfilled; this produces a warning. Zero hashes is an error.

Invocation extras `txhashCorpusHashes` and `txhashCorpusLedgers` record actual
pool size and contributing ledger count. The requested size is in invocation
flags. The same seed and dataset reproduce sampling; a finite pool can cause
cache reuse and is not a model of the full production working set.
