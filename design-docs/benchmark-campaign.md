# Benchmark Campaigns

| | |
|---|---|
| Status | Draft |
| Base | `feature/full-history` at `36ac502` |
| Replaces | [marwen-abid/stellar-rpc#12](https://github.com/marwen-abid/stellar-rpc/pull/12) to [marwen-abid/stellar-rpc#20](https://github.com/marwen-abid/stellar-rpc/pull/20) |
| Related | [stellar-rpc-benchmarks](https://github.com/stellar-experimental/stellar-rpc-benchmarks), [stellar-rpc-blaster](https://github.com/stellar/stellar-rpc-blaster) (branch `dev`) |

## 1. Purpose

A benchmark campaign answers three questions about RPCv2:

1. **Ingestion.** Does ingestion keep up with the ledger close interval?
2. **Queries.** Does each read endpoint meet its p99 latency target under
   the expected traffic, for recent data (hot tier) and older data (cold
   tier)?
3. **Cause.** When a request is slow, how much of its time is storage?

## 2. Who owns what

| Repository | Job | Contains |
|---|---|---|
| `stellar/stellar-rpc` | Measure. | The bench commands, the campaign runner, the GitHub Actions workflow, the EC2 box setup. |
| `stellar-experimental/stellar-rpc-benchmarks` | Judge. | The phases and targets (`docs/targets.json`), the converter, the verdicts, the results site. |

Rules:

- stellar-rpc knows nothing about phases, targets or verdicts. Its results
  are plain measurements.
- The benchmarks repository does not build or run stellar-rpc. It reads
  results bundles.
- The **results bundle** (Section 7) is the only contract between the two
  repositories.
- stellar-rpc pushes each finished bundle to the benchmarks repository.

## 3. Words

| Word | Meaning |
|---|---|
| Campaign | One dispatched run of the workflow, on one EC2 box. |
| Step | One command that the campaign runs, for example one `bench-ingest hot` run. |
| Close interval | The time between two ledgers that the hot ingestion step simulates: `2s`, `1s` or `600ms`. |
| Dataset profile | A synthetic ledger workload, for example `sac-6000`: SAC transfers, 6,000 transactions per ledger. |
| Dataset | The directory that one `bench-ingest` step writes: data files and a catalog. |
| Catalog | A small RocksDB database in the dataset. It records which chunks exist and where each one is stored. The daemon reads it to route each request. |
| Chunk | A block of 10,000 ledgers. Storage is organized per chunk. |
| Tier | Where a chunk is stored. **Hot**: a RocksDB database that ingestion still writes to. **Cold**: older chunks, converted into read-only files. |
| Load level | A total request rate for the load step, for example 500 requests per second. |
| Traffic mix | How the total rate is divided between the RPC methods. |
| Results file | The `results.json` file that one step writes. |
| Results bundle | All results files of one campaign, plus `campaign.json`. |
| Verdict | Pass or fail against a target. Only the benchmarks repository makes verdicts. |

## 4. A campaign from start to end

1. A person dispatches the `Bench campaign` workflow in stellar-rpc
   (Section 5).
2. The workflow checks the inputs, writes the campaign file, and starts an
   EC2 box. Then the workflow ends.
3. The box builds the stellar-rpc binary at the requested commit.
4. For each dataset profile and each run, the box runs these steps:
   1. `bench-ingest cold`: build a cold dataset from the ledger packs, and
      time it.
   2. `bench-ingest hot`: build a hot dataset at the close interval, and
      time it.
   3. Load step on the cold dataset: start `bench-serve`, run Blaster, save
      Blaster's results and the storage metrics.
   4. Load step on the hot dataset: the same.
5. The box writes the results bundle and uploads it to S3.
6. The box pushes the bundle to the benchmarks repository, posts a Slack
   message, and terminates itself.
7. In the benchmarks repository, a workflow converts the new bundle,
   applies the targets, and updates the site.

If a step fails, the box still does steps 5 and 6 for the completed steps.
The Slack message names the failed step.

## 5. Workflow inputs

| Input | Values | Default | Meaning |
|---|---|---|---|
| `ref` | branch, tag or commit | `feature/full-history` | The stellar-rpc commit to measure. |
| `close_interval` | `2s`, `1s`, `600ms` | required | Selects the close interval and the dataset profiles. |
| `steps` | any of `ingest-cold`, `ingest-hot`, `load` | all | The steps to run. `load` needs both ingest steps. |
| `runs` | number | 1 | Repetitions of each step. |
| `machine` | `2x`, `8x` | `2x` | `2x` = m6id.2xlarge, `8x` = c6id.8xlarge. |
| `workers` | number | per machine | `bench-ingest cold --workers`. |
| `hot_ledgers` | number | 0 (all) | Limit for the hot ingestion step. Use it for short test runs. |
| `name` | text | from the inputs | The campaign name. |
| `publish` | `yes`, `no` | `no` | Push the bundle to the benchmarks repository. Test runs keep `no`. |

The campaign runner comes from the workflow's own commit. There is no
input for the benchmarks repository.

For now, stellar-rpc hard-codes the dataset profiles for each close interval:

| Close interval | Dataset profiles |
|---|---|
| `2s` | `sac-6000`, `custom_token-4000`, `soroswap-1500` |
| `1s` | `sac-5000`, `custom_token-4000`, `soroswap-1500` |
| `600ms` | `sac-6000`, `custom_token-3600`, `soroswap-1800` |

These are the profiles that `render-campaign-toml.sh` (#15) uses today.
The benchmarks repository maps each close interval to its phase.

## 6. Components in stellar-rpc

### 6.1 `bench-ingest` (merged; two changes)

1. **Keep the catalog.** Today the command deletes its catalog when it exits
   (`bench/scratch.go`, `openScratchCatalog`). Write the catalog at
   `<dataset>/catalog/rocksdb` and keep it. Pin the earliest ledger to the
   first ledger of `--start-chunk`. A run into a directory that has a
   catalog fails before it writes.
2. **Write `results.json`** (Section 7.2). Keep the CSV files until the
   converter reads `results.json`, then remove them.

### 6.2 Read-only hot open

`bench-serve` must not change a hot dataset. Today a read-only open
(`hotchunk.OpenReadOnly`) cannot serve events, and a read-write open replays
the WAL and can compact.

- Add one function to `stores/hotchunk` that opens a chunk database
  read-only and builds the events facade.
- The comment in `stores/hotchunk/hotchunk.go` gives this work to #772.
  Tell the owner of #772 before the PR.
- Test: the directory's file set (names, sizes, modification times) is the
  same before and after an open, reads and a close.

### 6.3 `bench-serve` (new)

`bench-serve` serves a dataset over JSON-RPC. It does not ingest.

```
stellar-rpc-v2 bench-serve --dataset /data/ds-cold-sac-6000 \
  --listen 127.0.0.1:8000 --admin-listen 127.0.0.1:8001 \
  --network-passphrase "<passphrase of the dataset>"
```

1. Open the catalog and the data files. Open hot databases with the
   read-only open (Section 6.2). Use the daemon's own open sequence
   (`query.NewRegistry`, `PublishHandle`, `adapters.SeedCloseTimes`). Do not
   rebuild the catalog from file names.
2. Build the JSON-RPC handler with the code that the daemon uses
   (`rpcv2/jsonrpc.go`). Do not copy the method table.
3. Serve `getHealth`, `getNetwork`, `getLatestLedger`, `getLedgers`,
   `getTransactions`, `getTransaction` and `getEvents`. Other methods return
   "method not found".
4. Do not start captive core, ingestion, backfill or the lifecycle loop.
5. The dataset's close times are old. `getHealth` must report healthy.
6. `--admin-listen` serves `/metrics` and `/debug/pprof`.

### 6.4 Storage metrics (new, also in production)

Add to the shared read path:

| Metric | Labels | Meaning |
|---|---|---|
| `fullhistory_read_store_seconds` (histogram) | `method`, `store` (`ledgers`, `events`, `txhash`), `tier` (`hot`, `cold`) | Time that one request spent in one store and tier. |
| `fullhistory_read_open_seconds` (histogram) | `store` | Time to open a cold reader. |

- The read view adds up the time per store and tier. `ReadView.Release`
  records one observation for each store and tier that the request used.
- `wrapAdapterRequest` (`rpcv2/jsonrpc.go`) gives the method name to the
  read view.
- The metrics compare directly with `json_rpc_request_duration_seconds`.
- Document both metrics in `docs/MONITORING.md`.

### 6.5 Campaign runner (moved)

Today the runner lives in the benchmarks repository (`runner/cmd/campaign`),
and the box clones that repository. Move the runner into stellar-rpc, as
`cmd/bench-campaign`, and reduce it to the steps in Section 4.

- Input: the campaign file that the workflow writes.
- It builds the binary at `ref`, runs the steps, and writes the bundle.
- It builds Blaster at a pinned commit (Q1).
- It knows no targets. Load levels and the traffic mix come from a
  configuration file in stellar-rpc (Section 6.7).

### 6.6 Workflow and box (rework of #15 to #20)

The box finishes its own campaign. The workflow only starts it.

| Today (#15 to #20) | New |
|---|---|
| Four chained poll jobs wait up to about 21 hours for the box. | No polling. The workflow ends after the box starts. |
| The workflow clones the benchmarks repository with a personal token and runs its `ingest.sh` with AWS credentials. | The box pushes the bundle files only. It runs no code from the benchmarks repository. |
| The `phase` input, and a copy of the runner's query grid for the time estimate. | The `close_interval` input. The runner gives its own time estimate. |
| `notify`, `cleanup`, reaper: three places that decide pass or fail and terminate boxes. | The box posts Slack and terminates itself. The reaper terminates boxes past their deadline, as a safety net. |
| The box greps log lines (`published:`). | The runner writes `campaign.json` with the state of each step. |
| Any passing run publishes to the public site. | Only runs with `publish=yes` publish. |

The box needs two secrets: the benchmarks repository push token and the
Slack webhook. The box reads them from AWS (Q2). They are not in the
user-data.

Keep from #15 to #20: the input checks, the gzipped user-data, the awscli
fallback, the Slack message layouts, the reaper and their tests. Run those
tests in CI.

### 6.7 Load step

- Load levels: 250, 500 and 1,000 requests per second in total.
- Traffic mix: getTransaction 60%, getEvents 20%, getTransactions 15%,
  getLedgers 5%.
- These numbers are in `bench/campaign/load.toml` in stellar-rpc. Today
  they are the same as `docs/targets.json`. The copy is intentional: the
  campaign decides the load, and the benchmarks repository decides the
  targets.
- For each dataset, the step:
  1. starts `bench-serve`;
  2. runs `blaster generate` to collect real hashes, contracts and topics;
  3. reads `/metrics`;
  4. runs `blaster run`;
  5. reads `/metrics` again;
  6. stops `bench-serve`.

## 7. Results bundle

### 7.1 Layout

```
<campaign-id>/
  campaign.json
  steps/
    ingest-cold-sac-6000-run1/results.json
    ingest-hot-sac-6000-run1/results.json
    load-cold-sac-6000-run1/blaster.json
    load-cold-sac-6000-run1/metrics-before.txt
    load-cold-sac-6000-run1/metrics-after.txt
    load-hot-sac-6000-run1/...
  logs/
```

### 7.2 `results.json` (bench commands)

Example (numbers invented):

```json
{
  "schemaVersion": 1,
  "command": "bench-ingest hot",
  "binary": {"version": "v2.0.0-dev", "commit": "36ac502"},
  "startedAt": "2026-10-01T10:02:11Z",
  "finishedAt": "2026-10-01T10:24:40Z",
  "status": "ok",
  "error": "",
  "parameters": {"startChunk": 5120, "closeInterval": "2s", "ledgers": 10000, "source": "pack"},
  "measurements": [
    {"name": "ingest_total", "unit": "ms", "count": 10000,
     "p50": 410.2, "p90": 612.0, "p99": 880.4, "max": 1203.9, "total": 4151000.0},
    {"name": "peak_rss", "unit": "bytes", "count": 1, "value": 412000000}
  ]
}
```

Rules:

1. Each measurement has a name and a unit. A number never hides in a field
   for another unit.
2. The measurement names of `bench-ingest` are the row names it uses in its
   CSV files today.
3. Document the schema in a README in `cmd/stellar-rpc/internal/rpcv2/bench`.
   Increase `schemaVersion` on each incompatible change.

### 7.3 `campaign.json`

```json
{
  "schemaVersion": 1,
  "id": "cl2s-2x-20261001T1000Z",
  "inputs": {"ref": "feature/full-history", "closeInterval": "2s", "steps": ["ingest-cold", "ingest-hot", "load"],
             "runs": 1, "machine": "2x", "hotLedgers": 0},
  "commit": "36ac502",
  "machine": {"instanceType": "m6id.2xlarge", "cpus": 8, "memoryGiB": 32},
  "blasterCommit": "aadc1a1",
  "startedAt": "2026-10-01T10:00:03Z",
  "finishedAt": "2026-10-01T14:31:55Z",
  "steps": [
    {"name": "ingest-cold-sac-6000-run1", "status": "ok"},
    {"name": "load-hot-sac-6000-run1", "status": "failed", "error": "bench-serve: open hot chunk 5120: …"}
  ]
}
```

### 7.4 Blaster results

`blaster.json` is Blaster's own results file. It is not converted on the
box. Per endpoint, it holds: request, success and error counts; p50, p95,
p99 and p99.9 latency; errors by type; and one timeline entry per step of
the ramp. getEvents results are also split by traffic pattern.

## 8. Benchmarks repository changes

1. Add a workflow that runs when a bundle arrives. It converts the bundle
   into `docs/runs/<id>.json` and updates the site.
2. The converter reads `results.json`, `blaster.json` and the metrics files.
   It maps the close interval to a phase and applies `docs/targets.json`.
3. Remove the runner, its box setup and `scripts/ingest.sh` once stellar-rpc
   runs its own campaigns.
4. Until the load step works, the site shows no query verdicts.

## 9. What goes away

| Item | Where |
|---|---|
| `bench-query` and its request scheduler, rate rows and accounting file | #12 to #14 |
| The hand-built cold catalog in `bench-query` | #13 |
| The four poll jobs and the S3 result relay | #19 |
| The `phase` input and the copied query grid | #15 |
| The `benchmarks_ref` input and the clone of the benchmarks repository on the box | #16, #19 |
| The run of `ingest.sh` in the stellar-rpc workflow | #20 |
| The runner in the benchmarks repository | benchmarks `runner/` |

## 10. Delivery plan

### 10.1 stellar-rpc

| PR | Content | Done when |
|---|---|---|
| 1 | This spec. | The team agrees. |
| 2 | Read-only hot open with events (Section 6.2). | The file-set test passes. |
| 3 | `bench-ingest` keeps its catalog and writes `results.json` (Section 6.1). | A cold run and a hot run each leave a catalog and a `results.json`. |
| 4 | Storage metrics (Section 6.4). | One request records one observation per store and tier. The overhead is 2% or less. |
| 5 | `bench-serve` (Section 6.3). | A test calls each served method over HTTP, on a cold and a hot test dataset. |
| 6 | Campaign runner in `cmd/bench-campaign`, ingestion steps only (Section 6.5). | A local run writes a valid bundle. |
| 7 | Workflow and box (Section 6.6). | One campaign with `publish=no` runs on EC2 and posts to Slack. |
| 8 | Load step (Section 6.7). | One campaign produces `blaster.json` and metrics for a cold and a hot dataset. |

PRs 2, 3 and 4 can go in parallel. Start the stack from current
`feature/full-history`. The #15 to #20 stack is on an old copy of #936,
which was later merged as a single squashed commit.

### 10.2 stellar-rpc-benchmarks

| Change | After |
|---|---|
| The converter reads `results.json` for ingestion. | PR 3 |
| Workflow on bundle arrival. | PR 7 |
| The converter reads `blaster.json` and the metrics; query verdicts. | PR 8 |
| Remove the old runner. | PR 7 |

## 11. Code rules

The old stacks had these defects. Each PR must avoid them.

1. Each PR runs end to end and contains its own tests.
2. A doc comment says what the code does. It does not describe callers,
   history or rejected options.
3. Validate input one time, at the boundary.
4. Use one cleanup path per function.
5. Put one unit in each field.
6. Do not scrape log lines between programs. Write a file with fields.
7. Use one word for each concept (Section 3).
8. Keep each PR to about 600 lines of non-test code or fewer.

## 12. Open questions

| # | Question |
|---|---|
| Q1 | Blaster: its `main` branch is empty, and another team owns its code on `dev`. Do we pin a commit, or talk to the owners first? |
| Q2 | Box secrets: where in AWS do the push token and the Slack webhook live (Secrets Manager or Parameter Store)? Who adds them, and who gives the instance role access? |
| Q3 | Blaster's getEvents requests use page limits of 100 to 1,000. The targets assume 10. Force 10 in the Blaster config, or keep production shapes and review the targets? |
| Q4 | Blaster ramps linearly. Run it three times per dataset (one per load level), or run one ramp and read the 500 rps step? |
| Q5 | Blaster measures latency from the send time. Near saturation this hides queue delay. Is that acceptable for verdicts? |
| Q6 | Does `bench-serve` need a ready hot chunk above a cold range to get a read view? The old `bench-query` cold fixture added one. Check in PR 5. |
