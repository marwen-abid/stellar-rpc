# Benchmark Campaigns

| | |
|---|---|
| Status | Draft |
| Base | `feature/full-history` at `91f158b` (upstream; the fork was fast-forwarded to it on 2026-09-30) |
| Replaces | [marwen-abid/stellar-rpc#12](https://github.com/marwen-abid/stellar-rpc/pull/12) to [marwen-abid/stellar-rpc#20](https://github.com/marwen-abid/stellar-rpc/pull/20) |
| Decisions | [benchmark-campaign-decisions.md](./benchmark-campaign-decisions.md) |
| Overview | [RPCv2 Benchmark Campaigns](https://claude.ai/artifact/RWRgf15Ka54ySAzBAHB6k1) (one-page summary) |
| Related | [stellar-rpc-benchmarks](https://github.com/stellar-experimental/stellar-rpc-benchmarks), [stellar-rpc-blaster](https://github.com/stellar/stellar-rpc-blaster) (branch `dev`) |

## 1. Purpose

A benchmark campaign answers these questions about RPCv2:

1. **Ingestion.** Does hot ingestion finish each ledger within the ledger
   close interval? The campaign measures this over the last
   `paced_ledgers` ledgers of the hot dataset, after the earlier ledgers
   are in place (D21). Only hot ingestion has a target.
2. **Backfill and freeze.** How long does cold ingestion take per chunk?
   How long does the freeze of a hot chunk take? These times have no
   target.
3. **Queries.** Does each read endpoint meet its p99 latency target under
   the expected traffic? The campaign measures recent data (hot tier) and
   older data (cold tier) separately (D14).
4. **Cause.** When a request is slow, how much of its time is storage?

Each stage runs in isolation: hot ingestion, backfill, freeze and queries
are separate steps. Profiling is available in every step (Section 6.9).
`bench-live` (Section 6.10) runs ingestion and queries in one process. The
campaign does not use it yet.

### 1.1 Not measured

- Hot latency while ingestion writes. The hot load step serves a finished
  hot dataset. No ingestion runs at the same time (D3, D14). `bench-live`
  covers this case later (D25).
- Discard and prune, the lifecycle stages after the freeze.
- Cold tx-hash lookups against a production-size index. A bench dataset
  holds 2 chunks. Its index holds 2 × 10,000 × (transactions per ledger)
  keys and fits in the page cache. A production index covers 1,000 chunks
  (Q9).
- A live head. The head of a dataset does not move during a load step.
  Near-head requests read the same ledgers again and again.

Query numbers are for a server that shares the box with Blaster (D22).

## 2. Who owns what

| Repository | Job | Contains |
|---|---|---|
| `stellar/stellar-rpc` | Measure. | The bench commands, the campaign runner, the GitHub Actions workflow, the EC2 box bootstrap and the box scripts. |
| `stellar-experimental/stellar-rpc-benchmarks` | Judge. | The phases and targets (`docs/targets.json`), the converter, the verdicts, the results site. |

Rules:

- stellar-rpc knows nothing about phases, targets or verdicts. Its results
  are plain measurements.
- The benchmarks repository does not build or run stellar-rpc. It reads
  results bundles.
- The **results bundle** (Section 7) is the only contract between the two
  repositories.
- Every campaign uploads its bundle to S3. A campaign with `publish=yes`
  also pushes its bundle to the benchmarks repository (D11, D17).

## 3. Words

Use one word for each concept (D19). Do not use these words:
`corpus`, `leg`, `cell`, `shed`, `pacing`, `fixture`. Do not use "phase"
in stellar-rpc code, flags or files. This spec uses it only when it
describes the benchmarks repository. Code identifiers in backticks are not
affected.

| Word | Meaning |
|---|---|
| Campaign | One dispatched run of the workflow, on one box. |
| Campaign file | `campaign.json`. The workflow writes it. The runner adds to it after each step (Section 7.3). |
| Box | The EC2 instance of one campaign. The `machine` input selects its type. |
| Step | One unit of work that the runner runs and records, for example one `bench-ingest hot` run. |
| Step kind | `ingest-cold`, `ingest-hot`, `freeze` or `load`. |
| Step name | `<kind>-<profile>-run<N>`, for example `ingest-cold-sac-6000-run1`. A load step adds the tier: `load-<tier>-<profile>-run<N>`. |
| Close interval | The time between two ledgers that the hot ingestion step simulates: `2s`, `1s` or `600ms`. |
| Dataset profile | A synthetic ledger workload, for example `sac-6000`: SAC transfers, 6,000 transactions per ledger. |
| Ledger pack | An input file of `bench-ingest --source pack`. The campaign uses packs in the `packs-v2` layout. |
| Dataset | The directory that one `bench-ingest` step writes: data files and a catalog. |
| Test dataset | A small dataset that a Go test builds. |
| Catalog | A small RocksDB database in the dataset, at `<dataset>/catalog/rocksdb`. It records which chunks exist and their state. The read path uses it to route each request. |
| Chunk | A block of 10,000 ledgers. Chunk `c` starts at ledger `c × 10000 + 2`. Storage is organized per chunk. |
| Tier | Where a chunk is stored. **Hot**: a chunk in its own RocksDB database, written by hot ingestion and not yet converted to cold files. A hot dataset from `bench-ingest hot --num-chunks N` holds N hot chunks. `bench-serve` opens all of them. **Cold**: a chunk converted to read-only files. |
| Frontier chunk | The empty hot chunk that `bench-ingest cold` adds after its last chunk. The read path needs one ready hot chunk to create a read view (D4). |
| Paced ledgers | The last N ledgers of a hot dataset. Hot ingestion commits them at the close interval and times them (D21). |
| Load level | A total request rate for the load step, for example 500 requests per second. |
| Traffic mix | How the total rate is divided between the RPC methods. |
| Window | One `--step-interval` period of a Blaster run (default 5 seconds). |
| Archetype | One of Blaster's getEvents request shapes, for example `head-poll` or `deep-pager`. |
| Results file | The file that one step writes into its step directory: `results.json` for a bench command, `load.json` for a load step. |
| Results bundle | All step directories of one campaign, plus `campaign.json`. |
| Verdict | Pass or fail against a target. Only the benchmarks repository makes verdicts. |

## 4. A campaign from start to end

1. A person dispatches the `Bench campaign` workflow in stellar-rpc
   (Section 5).
2. The workflow checks the inputs. It builds `cmd/bench-campaign` at its
   own commit and runs `bench-campaign plan` to write `campaign.json`. It
   runs `bench-campaign estimate campaign.json` to set the box ceiling and
   the `deadline` tag (Section 6.6). It starts the box and prints the
   instance id. Then the workflow ends.
3. The box sets its `shutdown -P` ceiling. It clones stellar-rpc at the
   workflow commit and runs the box bootstrap (Section 6.6).
4. The runner builds the stellar-rpc binary at `ref` and builds Blaster at
   its pinned commit (Section 6.5).
5. For each dataset profile, the runner fetches the ledger packs with
   `aws s3 sync` into `<root>/<profile>/packs`. It records the byte count
   and the duration in `campaign.json`.
6. For each run N of the profile, the runner runs these steps. Each run has
   its own datasets: `<root>/<profile>/run<N>/cold` and
   `<root>/<profile>/run<N>/hot`.
   1. `ingest-cold`: `bench-ingest cold` builds a cold dataset of 2 chunks
      from the packs, and times it.
   2. `ingest-hot`: `bench-ingest hot` builds a hot dataset of 2 chunks. It
      paces the last `paced_ledgers` ledgers at the close interval.
   3. `load-cold`: the load step on the cold dataset of run N
      (Section 6.7).
   4. `load-hot`: the load step on the hot dataset of run N.
   5. `freeze`: `bench-ingest freeze` converts the hot dataset of run N to
      cold files, and times it (Section 6.8). It runs last because it
      changes the hot dataset.
7. The `steps` input selects which of these steps run (Section 5). A load
   step of tier T in run N always serves the dataset that step
   `ingest-T` of run N wrote.
8. After each step, the runner rewrites `campaign.json`. The box uploads
   `campaign.json` and the finished step directory to S3.
9. After the last step of a profile, the runner removes the datasets and
   the packs of that profile.
10. The box finishes the campaign in this order: S3 upload, push
    (`publish=yes` only), Slack message, box log upload, `poweroff`.
11. In the benchmarks repository, a workflow converts the new bundle,
    applies the targets and updates the site (Section 8).

Failure rules (D16):

- The box shell script, not the runner, does steps 8 and 10.
- If the runner exits non-zero or is killed, the shell marks the running
  step `crashed`. Then it continues with step 10.
- A push failure or a Slack failure does not stop the `poweroff`.
- If the S3 upload fails, the box stays up until its ceiling. The Slack
  message says so.
- The Slack message names each failed step.

## 5. Workflow inputs

The workflow has nine inputs.

| Input | Values | Default | Meaning |
|---|---|---|---|
| `ref` | branch, tag or commit | `feature/full-history` | The stellar-rpc commit to measure. The runner comes from the workflow's own commit. |
| `close_interval` | choice: `2s`, `1s`, `600ms` | required | Selects the close interval and the dataset profiles. |
| `steps` | choice: `ingest-cold`, `ingest-hot`, `ingest`, `cold`, `all` | `all` | The steps to run. See the table below. |
| `runs` | 1 to 5 | 1 | Repetitions of each step. At `2s`, one run of the three profiles has about 17 hours of paced hot ingestion (3 × 10,000 × 2 s). |
| `machine` | choice: `2x`, `8x` | `2x` | `2x` = m6id.2xlarge, `8x` = c6id.8xlarge. |
| `workers` | 1 to 128 | 8 for `2x`, 32 for `8x` | `bench-ingest cold --workers` and `bench-ingest freeze --workers`. |
| `paced_ledgers` | 0 to 20,000 | 10000 | `bench-ingest hot --paced-ledgers`. 0 paces all ledgers of the hot dataset. Use a small value for short test runs. |
| `name` | letters, digits, `.`, `_`, `-` | from the inputs | The campaign name. The campaign id is `<name>-<GitHub run id>`. |
| `publish` | choice: `yes`, `no` | `no` | Push the bundle to the benchmarks repository. Test runs keep `no`. |

There is no input for the benchmarks repository or for the packs location.
`cmd/bench-campaign/load.toml` holds the packs S3 prefix (Section 6.7).

| `steps` | Steps that run in each run |
|---|---|
| `ingest-cold` | `ingest-cold` |
| `ingest-hot` | `ingest-hot`, `freeze` |
| `ingest` | `ingest-cold`, `ingest-hot`, `freeze` |
| `cold` | `ingest-cold`, `load-cold` |
| `all` | `ingest-cold`, `ingest-hot`, `load-cold`, `load-hot`, `freeze` |

For now, `cmd/bench-campaign` hard-codes the dataset profiles for each
close interval:

| Close interval | Dataset profiles |
|---|---|
| `2s` | `sac-6000`, `custom_token-4000`, `soroswap-1500` |
| `1s` | `sac-5000`, `custom_token-4000`, `soroswap-1500` |
| `600ms` | `sac-6000`, `custom_token-3600`, `soroswap-1800` |

These are the profiles that `render-campaign-toml.sh` (#15) uses today. The
same Go table holds, for each profile, `start_chunk` and the network
passphrase of the packs. The benchmarks repository maps each close
interval to its phase.

Dataset shape (D21):

- The cold dataset covers 2 chunks from `start_chunk`. Blaster needs more
  than 10,000 ledgers of retention for its `deep-pager` archetype.
- The hot dataset covers the same 2 chunks.
- The runner fails a step when the packs of its profile do not cover the
  2 chunks. Today the `sac-6000` packs cover one chunk (Q7).

## 6. Components in stellar-rpc

### 6.1 `bench-ingest` (merged; four changes)

1. **Keep the catalog.** Today the command deletes its catalog when it
   exits (`bench/scratch.go`, `openScratchCatalog`).
   - Before the command opens anything, it checks that
     `<dataset>/catalog/rocksdb` does not exist. If it exists, the command
     exits with an error and writes no file.
   - Write the catalog at `<dataset>/catalog/rocksdb` and keep it. The
     dataset is `--cold-out-dir` for `cold` and `--hot-dir` for `hot`.
   - Pin the earliest ledger to the first ledger of `--start-chunk`
     (`chunk × 10000 + 2`) with `PinEarliestLedger`. `bench-serve` reads it
     to build the retention window.
   - `bench-ingest cold` also creates an empty hot chunk database for the
     frontier chunk (last chunk + 1) under `<dataset>/hot/`. Use the
     catalog's hot create sequence (`BeginHotCreate`, `hotchunk.Open`,
     `FinishHotCreate`), as the hot ingestion loop does. This marks the
     chunk `ready`. A live node whose next chunk has no ledgers yet is in
     the same state (D4).
2. **Write `results.json`** (Section 7.2). Keep the CSV files until the
   converter reads `results.json` (B1). Then remove them (PR 11).
3. **Add `--paced-ledgers N` to `bench-ingest hot`** (default 10000; 0 =
   all).
   - Ledgers before the last N of the range ingest back to back. They are
     not measured.
   - `ingest_total`, `pace_lag` and `run_wall` cover the paced ledgers
     only.
   - `results.json` records `pacedLedgers` and `unpacedLedgers` in
     `parameters`.
4. **Campaign flags.** Both ingest steps run with `--source pack --pack-dir
   <root>/<profile>/packs`. The hot step always passes `--close-interval`.

### 6.2 Read-only opens

`bench-serve` must not change a dataset (D5). Today a read-only hot open
(`hotchunk.OpenReadOnly`) has no events facade, so hot `getEvents` fails.
A read-write open rewrites the RocksDB housekeeping files and creates a new
WAL file, even when the WAL is empty. `catalog.Open` is read-write only.

- Add one function to `stores/hotchunk` that opens a chunk database
  read-only and builds the events facade.
- Add `catalog.OpenReadOnly`. It uses `rocksdb.Config.ReadOnly`. It fails
  when the catalog does not exist.
- Remove the comments that say a read-only open is ledgers-only, in
  `stores/hotchunk/hotchunk.go` and `query/resolve.go`.
- A read-only open takes no LOCK. Never open a dataset read-only while a
  writer has it open.
- Test: the file set of a hot chunk directory (names, sizes, modification
  times) is the same before and after an open, reads (events included)
  and a close.
- Test (PR 05): the same check runs over the whole dataset root when
  `bench-serve` opens a dataset, serves reads and closes.

### 6.3 `bench-serve` (new)

`bench-serve` serves a dataset over JSON-RPC. It does not ingest.

```
stellar-rpc-v2 bench-serve --dataset /mnt/nvme/bench/sac-6000/run1/cold \
  --listen 127.0.0.1:8000 --admin-listen 127.0.0.1:8001 \
  --network-passphrase "<passphrase of the packs>"
```

Code location (D26): the wiring is an exported entry point in package
`rpcv2`, for example `rpcv2.ServeDataset(ctx, opts)`. The flags and the
cobra command are in `rpcv2/bench`. `cmd/stellar-rpc/rpcv2/main.go`
registers the command next to `bench-ingest`.

Start sequence:

1. Fail when `--network-passphrase` is empty.
2. Build the layout with `geometry.NewLayout(<dataset>)`. Open the catalog
   with `catalog.OpenReadOnly` (Section 6.2).
3. Read `config:earliest_ledger`. Build the retention window with size 0
   and the chunk of that ledger as the earliest chunk.
4. Assemble the registry with `query.NewRegistry`. Do not call
   `query.OpenRegistry`: it opens hot chunks read-write and gates
   `getHealth` on a first commit.
5. Open every chunk that the catalog marks `ready` with the read-only open
   of Section 6.2. Call `PublishHandle` for each one.
6. Get the latest ledger as the daemon does (`lastCommittedLedger`: the
   highest durable cold chunk, refined by the highest ready hot database).
   Call `SetLatestLedger` with it. Then call `adapters.SeedCloseTimes`.
7. Build the configuration with `config.ParseConfig(nil)`. Set
   `[service].endpoint`, `[service].admin_endpoint` and
   `[service.methods.getHealth].max_healthy_ledger_latency`. Set the
   latency limit high, so that `getHealth` reports healthy on old close
   times.
8. Build the JSON-RPC handler with the daemon's code
   (`newJSONRPCHandler`, `handlerParams`). Do not copy the method table.
   Pass a non-nil `*feewindow.FeeWindows`.
9. Filter the method table after `LimitsByMethod.Apply`. Serve only these
   seven methods (D30): `getHealth`, `getNetwork`, `getLatestLedger`,
   `getLedgers`, `getTransactions`, `getTransaction`, `getEvents`. Other
   methods return "method not found" (-32601).
10. Register the Go process collectors and the serving collectors on one
    Prometheus registry, as `rpcv2/daemon.go` does. Pass a Daemon whose
    `MetricsRegistry()` returns that registry. `host.MakeNoOpDaemon` does
    not do this: it returns a new registry on each call.
11. Serve `/metrics` and `/debug/pprof` on `--admin-listen`
    (`startAdminServer`).
12. Log one line `ready` after the listeners are open.

Rules:

- Do not start captive core, ingestion, backfill or the lifecycle loop.
- If any open fails, exit non-zero.
- On SIGTERM, close the listeners, the databases and the catalog. Then
  exit 0.
- `--profile-rates` sets the block and mutex profile rates (Section 6.9).

### 6.4 Storage metrics (new, also in production)

Add to the shared read path (D6):

| Metric | Labels | Meaning |
|---|---|---|
| `fullhistory_read_store_seconds` (histogram) | `method`, `store` (`ledgers`, `events`, `txhash`), `tier` (`hot`, `cold`) | Time that one request spent in one store and tier. |
| `fullhistory_read_open_seconds` (histogram) | `store` | Time to open a cold reader. |

Both metrics use the `soroban_rpc` namespace, as the other rpcv2 metrics
do.

Rules:

- The read view adds up the time per store and tier. `ReadView.Release`
  records one observation for each store and tier that the request used.
- `wrapAdapterRequest` (`rpcv2/jsonrpc.go`) gives the method name to the
  read view. Use the method names that the `endpoint` label of
  `soroban_rpc_json_rpc_request_duration_seconds` uses, so the series join.
- A view with no method name records nothing. Examples: the startup fee
  replay (`feereplay.go`) and `adapters.SeedCloseTimes`.
- Time the store's own work only: each iterator step and the reader's
  first-use initialization. Do not include the handler body that runs
  inside the iterator's yield.
- Cold readers open per call. There is no reader cache.
  `ledger.OpenColdReader` and `event.OpenColdReader` do no I/O. Observe
  `fullhistory_read_open_seconds` at the reader's first blocking call
  (`init`, `waitOpen`, the MPHF load). For `txhash`, observe it at
  `txhash.OpenColdReader`.
- Open time also counts in `fullhistory_read_store_seconds`.
- `resolveLedgers` and `ReadView.Events` do not return the tier today. PR 04
  adds it.
- In production the `method` label takes the 13 registered method names.
  The series count is 13 × 3 × 2 × (bucket count) for the store histogram.
  PR 04 sets the bucket layout.

Storage share per method = Δ `_sum` of `fullhistory_read_store_seconds` ÷
Δ `_sum` of `soroban_rpc_json_rpc_request_duration_seconds`, for the same
method, between the two `/metrics` dumps of a Blaster run. The request
metric is a Summary. Its quantiles do not compare with the histogram. After
a duration-limiter timeout the handler keeps running, so its storage
observation can arrive after the response.

Document both metrics in `docs/MONITORING.md` and in the metrics table of
`docs/ARCHIVE-NODE-BETA-RUNBOOK.md`.

### 6.5 Campaign runner (new)

Write `cmd/bench-campaign` in stellar-rpc (D15). Take only these parts
from the benchmarks runner (`runner/cmd/campaign`): `internal/publish`, the
step loop, the bundle writers, the config validation and the lock. Do not
take the phases, the targets, the bench-query steps, the golden
preparation or `--resume`.

Subcommands:

| Subcommand | Where it runs | Job |
|---|---|---|
| `plan` | GitHub runner | Checks the inputs and writes `campaign.json`. |
| `validate` | anywhere | Checks a `campaign.json` against its schema. |
| `estimate <campaign.json>` | GitHub runner | Prints the expected campaign time in minutes. |
| `run <campaign.json>` | box or laptop | Runs the steps and writes the bundle. |

Rules:

- The Go runner plans, estimates, builds `ref`, runs the steps and writes
  the bundle. The box shell script uploads to S3, pushes to the benchmarks
  repository, posts to Slack and powers off (D15, D16).
- `run` builds the binary at `ref` in a second source tree. It records
  `commit` and `runnerCommit` in `campaign.json`.
- After the build, the runner checks that the binary has `bench-ingest`
  and `bench-serve`. If the binary has no `bench-ingest freeze`, the runner
  marks each freeze step `skipped` (D33).
- The runner builds Blaster at a pinned commit (Q1). Blaster needs Go 1.25
  or later, `jq` and `git`.
- The runner writes each step's stdout and stderr to
  `logs/<step name>.log`, and its own log to `logs/runner.log`.
- After each step, the runner rewrites `campaign.json` (temporary file and
  rename).
- The runner knows no targets. Load levels and the traffic mix come from
  `cmd/bench-campaign/load.toml` (Section 6.7, D28).
- For local runs, `run` accepts the dataset root (NVMe or EBS), the page
  cache drop (on or off) and the profile option.

The estimate is the sum of these terms:

- a fixed setup margin;
- the pack fetch: pack bytes at 100 MB/s;
- for each profile and run: cold ingestion time, plus unpaced ledgers ×
  an assumed back-to-back time per ledger, plus paced ledgers × close
  interval, plus the load steps (Blaster run duration × runs per dataset,
  plus the generate time), plus the freeze time.

`load.toml` holds the assumed times in an `[estimate]` table. The workflow
sets the box ceiling to estimate + 60 minutes, and the `deadline` tag to
ceiling + 30 minutes.

### 6.6 Workflow and box (rework of #15 to #20)

The box finishes its own campaign. The workflow only starts it.

| Today (#15 to #20) | New |
|---|---|
| Four chained poll jobs wait up to about 21 hours for the box. | No polling. The workflow ends after the box starts. |
| The workflow clones the benchmarks repository with a personal token and runs its `ingest.sh` with AWS credentials. | The box pushes the bundle files only. It runs no code from the benchmarks repository. |
| The `phase` input, and a copy of the runner's query grid for the time estimate. | The `close_interval` input. The workflow runs `bench-campaign estimate` for the box ceiling and the `deadline` tag. |
| The notify job decides pass or fail. Boxes terminate in four ways: box `poweroff`, the self-terminate ceiling, the cleanup job and the reaper. | The box posts to Slack and powers off. The self-terminate ceiling and the reaper stay as safety nets. |
| The box greps log lines (`published:`). | The runner writes `campaign.json` with the state of each step. |
| Any run with verdict `ok` publishes to the public site. | Only runs with `publish=yes` publish. |
| The benchmarks repository's `runner/bootstrap.sh` sets up the box. | stellar-rpc owns the box bootstrap (see below). |

**Box bootstrap.** Port benchmarks `runner/bootstrap.sh` into
`perf-eval/bench-campaign` (`perf-eval` is
`cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval`).
It does these tasks: instance-store discovery, `mkfs` and mount at
`/mnt/nvme`, the fsync probe, apt packages, the AWS CLI, pinned Go and
Rust, zstd and RocksDB. The dataset root is `/mnt/nvme/bench`.

**User-data.** The user-data is a fetch-and-run stub. Its first line after
the preamble sets the `shutdown -P` ceiling. Then it clones stellar-rpc at
the workflow commit, runs the bootstrap and runs the box script. The stub
is gzipped and must stay under 16 KB. `campaign.json` travels in the
user-data.

**Box script.** The box script runs `bench-campaign run` and finishes the
campaign (Section 4, D16):

1. After each step, it uploads `campaign.json` and the finished step
   directory to `s3://stellar-rpc-bench/results/<id>/` (D31).
2. At the end, it uploads the whole bundle and `logs/`.
3. With `publish=yes`, it pushes the bundle files, without `logs/`, to
   branch `bundle/<id>` of the benchmarks repository. It retries a
   rejected push up to 5 times.
4. It posts one Slack message. The message comes from `campaign.json`. It
   holds the campaign id, the status of each step, the S3 prefix, the log
   key and the push result. On a push failure it gives the S3 path.
5. It uploads the box log. Then it runs `poweroff`.

**Secrets.** The box needs two secrets: the push token for the benchmarks
repository and the Slack webhook. The box reads them from AWS (Q2). They
are not in the user-data.

**Reaper.** The reaper stays. It terminates boxes tagged `rpc-bench=true`
and `test=stellar-rpc-ci-load-test` with a numeric `deadline` in the past.
Its schedule fires only from `main`. Until the workflow is on `main`, run
the reaper by manual dispatch. The box's `shutdown -P` ceiling is the
in-band safety net.

**Workflow on `main`.** GitHub lists a `workflow_dispatch` workflow only
when a file exists at its path on the default branch. The inputs come from
the file on the selected ref. Check that the #922 stub is on `main`. If it
is not, add a stub.

Keep from #15 to #20: the input checks, the gzipped user-data, the awscli
fallback, `launch-box.sh` (AMI, tags), `post-slack.sh`, `slack-reaper.jq`,
the reaper and the tests for these parts. Rewrite `slack-payload.sh` and
`slack-campaign.jq` to read `campaign.json`. Add a CI job that runs the
script tests (Python 3.11 or later, bash, jq).

### 6.7 Load step

`cmd/bench-campaign/load.toml` holds these values (D28):

- Load levels: 250, 500 and 1,000 requests per second in total.
- Traffic mix: getTransaction 60%, getEvents 20%, getTransactions 15%,
  getLedgers 5%.
- The Blaster run duration: 60 seconds until Q4 is decided.
- The `blaster generate --count` value: 200.
- One `rng_seed` for each load level: 1, 2 and 3.
- The packs S3 prefix:
  `s3://stellar-rpc-bench/inputs/synthetic-ledgers/2026-07-18-apply-load-20k`.
  Packs are at `<prefix>/<profile>/packs-v2/cold`.
- `profile = true|false` (Section 6.9).
- The `[estimate]` table (Section 6.5).

Today the levels and the mix are the same as in `docs/targets.json`. The
copy is intentional: the campaign decides the load, and the benchmarks
repository decides the targets.

Blaster takes an integer rps for each method. The runner renders each
level to integers:

| Level | getTransaction | getEvents | getTransactions | getLedgers |
|---|---|---|---|---|
| 250 | 150 | 50 | 37 | 13 |
| 500 | 300 | 100 | 75 | 25 |
| 1,000 | 600 | 200 | 150 | 50 |

Q4 decides how many Blaster runs a load step makes and which level the
verdict uses. Each Blaster run uses one level, its `rng_seed` and its own
subdirectory `<level>rps/`.

Steps of one load step, in order (D22):

1. Start `bench-serve` on the dataset. Send its stdout and stderr to
   `<step dir>/bench-serve.log`.
2. Poll `getHealth` on `--listen` one time each second until it answers.
   Stop at 30 minutes and fail the step. Record the wait as
   `serveReadySeconds` in `load.json`.
3. Run `blaster generate --rpc-url http://127.0.0.1:8000 --ledger-window
   <first>,<last> --count <n> --output <step dir>/seed.json`. `<first>`
   and `<last>` are the ledger range of the dataset.
4. Send one `getTransaction` for a hash from `seed.json`. If the answer is
   `NOT_FOUND`, fail the step. A wrong passphrase causes this (D20).
5. For each Blaster run:
   1. Cold dataset only: stop `bench-serve` (rule of item 6). Drop the OS
      page cache: `sync; echo 3 > /proc/sys/vm/drop_caches` (the box runs
      as root). Start `bench-serve` again and wait as in items 1 and 2.
      The hot dataset keeps the cache and the running `bench-serve`.
   2. Write `<level>rps/blaster.toml`: `rng_seed` and one
      `[endpoints.<method>]` table with `rps` for each method.
   3. Save `/metrics` from `--admin-listen` to
      `<level>rps/metrics-before.txt`.
   4. Run `blaster run --rpc-url http://127.0.0.1:8000 --config-path
      <level>rps/blaster.toml --input-data-path <step dir>/seed.json
      --duration <run duration> --error-percent 100 --test-output-path
      <level>rps/blaster.json`. Do not set `--ramp-up`: without it, Blaster
      sends at a constant rate. Keep `--step-interval` at its default.
      Blaster requires `--duration`: a run without it sends nothing and
      exits 0.
   5. While Blaster runs, watch the `bench-serve` process. If it exits,
      stop Blaster. Record `status: failed` and
      `error: "bench-serve exited: <code>"` in `load.json`. Keep the
      partial `blaster.json`.
   6. With `profile = true`, save one CPU profile from
      `/debug/pprof/profile` during the run to `<level>rps/cpu.pprof`.
   7. Save `/metrics` again to `<level>rps/metrics-after.txt`.
6. Stop `bench-serve`: send SIGTERM, wait at most 60 seconds, then send
   SIGKILL. Record the exit code in `load.json`. A non-zero exit code
   fails the step.
7. Write `load.json` (Section 7.1).

Conditions (D22):

- The targets in `docs/sla-derivation.md` assume a dropped cache for cold
  and a warm cache for hot. `load.json` records `pageCache: dropped|kept`.
- Blaster and `bench-serve` share the box. `load.json` records the
  GOMAXPROCS and the CPU time of both processes. Core pinning is Q8.
- The `--step-interval` default is 5 seconds. A value of 0 gives one window
  per 5 ns (a unit defect in Blaster). Report it to the Blaster owners.

### 6.8 `bench-ingest freeze` (new)

`bench-ingest freeze` times the lifecycle freeze over a kept hot dataset
(D23).

```
stellar-rpc-v2 bench-ingest freeze --dataset /mnt/nvme/bench/sac-6000/run1/hot \
  --workers 8 --out <step dir>
```

- It opens the kept catalog read-write. The freeze writes cold files and
  catalog entries into the dataset.
- It runs `backfill.RunBackfill` over the complete ready hot chunks. The
  source of each chunk is its ready hot chunk database
  (`hotchunk.OpenReadyView`), as in the lifecycle freeze.
- It reuses the driver and the sink of `bench-ingest cold`, so the
  measurement names are the same.
- It writes `results.json` with `command: freeze`.
- It does not discard the hot chunks.

### 6.9 Profiling

- Add `--profile-rates <block ns>,<mutex fraction>` to a flag set that
  `bench-ingest`, `bench-serve` and `bench-live` share (D24). It calls
  `runtime.SetBlockProfileRate` and `runtime.SetMutexProfileFraction`. The
  default `0,0` turns both off.
- `bench-ingest` keeps `--cpuprofile` and `--memprofile`.
- `bench-serve` and `bench-live` serve `/debug/pprof` on the admin port.
- With `profile = true` in `load.toml`, the runner saves one CPU profile
  per step as `cpu.pprof` in the step directory. For a bench command it
  passes `--cpuprofile`. For a load step it follows Section 6.7.

### 6.10 `bench-live` (new)

`bench-live` runs the daemon body over a paced pack source, so Blaster can
load a node while it ingests at the close interval (D25).

- It runs the body of `startup.go` `run()`: `OpenRegistry`, the ingestion
  loop, the lifecycle and `ServeReads`.
- It injects a paced pack source through the `daemonOptions` seams `Core`,
  `Backend` and `ServeReads`, as `e2e_test.go` does.
- It takes `--network-passphrase`. An injected core leaves the passphrase
  empty, so the command must set it.
- It is read-write, and its lifecycle runs. These guarantees differ from
  `bench-serve`, so it is a separate command.
- The first delivery is the command and a local test. Campaign use is a
  later step.

## 7. Results bundle

### 7.1 Layout

```
<campaign-id>/
  campaign.json
  steps/
    ingest-cold-sac-6000-run1/
      results.json
    ingest-hot-sac-6000-run1/
      results.json
    load-cold-sac-6000-run1/
      load.json
      bench-serve.log
      seed.json
      500rps/
        blaster.toml
        blaster.json
        metrics-before.txt
        metrics-after.txt
    load-hot-sac-6000-run1/
      ...
    freeze-sac-6000-run1/
      results.json
  logs/
    <step name>.log
    runner.log
    box.log
```

Until PR 11, the ingest step directories also hold the CSV files. The push
to the benchmarks repository leaves out `logs/`.

The runner writes `load.json` for each load step. Example (numbers
invented):

```json
{
  "schemaVersion": 1,
  "step": "load-cold-sac-6000-run1",
  "blasterCommit": "aadc1a1…",
  "serveReadySeconds": 41.2,
  "benchServeExitCode": 0,
  "gomaxprocs": {"benchServe": 8, "blaster": 8},
  "startedAt": "2026-10-01T12:02:11Z",
  "finishedAt": "2026-10-01T12:09:40Z",
  "status": "ok",
  "error": "",
  "runs": [
    {"loadLevelRps": 500, "rngSeed": 2,
     "mix": {"getTransaction": 300, "getEvents": 100, "getTransactions": 75, "getLedgers": 25},
     "blasterArgs": ["run", "--duration", "60s", "--error-percent", "100", "…"],
     "pageCache": "dropped", "serveFresh": true,
     "cpuSeconds": {"benchServe": 212.4, "blaster": 97.1},
     "status": "ok", "error": ""}
  ]
}
```

`serveFresh` is true when `bench-serve` started just before the Blaster
run. Its RocksDB block cache is then empty.

### 7.2 `results.json` (bench commands)

Example for `bench-ingest hot` (numbers invented):

```json
{
  "schemaVersion": 1,
  "command": "ingest-hot",
  "binary": {"version": "v2.0.0-dev", "commit": "91f158b45951545923c0f74d81a6b8be33aed683"},
  "startedAt": "2026-10-01T10:02:11Z",
  "finishedAt": "2026-10-01T16:02:40Z",
  "status": "ok",
  "error": "",
  "parameters": {"startChunk": 1, "numChunks": 2, "closeInterval": "2s",
                 "pacedLedgers": 10000, "unpacedLedgers": 10000,
                 "source": "pack", "packFormat": "packs-v2"},
  "measurements": [
    {"name": "ingest_total", "kind": "summary", "unit": "ms", "count": 10000, "items": 10000,
     "p50": 410.2, "p90": 612.0, "p99": 880.4, "max": 1203.9, "total": 4151000.0},
    {"name": "pace_lag", "kind": "summary", "unit": "ms", "count": 10000, "items": 10000,
     "p50": 0.4, "p90": 1.1, "p99": 9.8, "max": 40.2, "total": 8100.0},
    {"name": "run_wall", "kind": "summary", "unit": "ms", "count": 1, "items": 10000,
     "p50": 20001000.0, "p90": 20001000.0, "p99": 20001000.0, "max": 20001000.0, "total": 20001000.0},
    {"name": "peak_rss", "kind": "value", "unit": "bytes", "value": 412000000}
  ]
}
```

Rules:

1. Each measurement has a name and a unit. Put one unit in each field.
2. The measurement names of `bench-ingest` are its CSV row names without a
   unit suffix. For example, `peak_rss_bytes` becomes `peak_rss` with
   `unit: bytes`.
3. Each measurement has a `kind`. `summary` has `count`, `items`, `p50`,
   `p90`, `p99`, `max` and `total`. `value` has `value`.
4. `items` is the item count of the CSV `n_items` column. The converter
   derives `ledgers_per_s` from it.
5. `binary.commit` is the full commit hash. It replaces `commitHash`.
6. `command` is the step kind: `ingest-cold`, `ingest-hot` or `freeze`.
7. `status` is `running`, `ok` or `failed`. The command writes the file
   with `status: running` when it starts, and again when it ends. Each
   write goes to a temporary file, then a rename.
8. `ingest_total` is the processing time of one ledger, from extract to
   apply. It does not include the source read or the wait for the due
   time. `pace_lag` is the delay of each commit after its due time. Its
   maximum and its trend answer question 1.
9. `bench-ingest cold` adds `txhashIndexChunks` to `parameters`: the number
   of chunks that its tx-hash index covers.
10. Document the schema in a README in `cmd/stellar-rpc/internal/rpcv2/bench`.
    Link it from `cmd/bench-campaign/README.md`. Increase `schemaVersion`
    on each incompatible change (D29).

### 7.3 `campaign.json`

Example (numbers invented). The workflow writes `schemaVersion`, `id`, `inputs`, `packsPrefix`, `load`
(a copy of `load.toml`) and `profiles`. The runner adds `commit`,
`runnerCommit`, `machine`, `blasterCommit`, `startedAt`, `finishedAt` and
`steps`.

```json
{
  "schemaVersion": 1,
  "id": "cl2s-2x-18234567890",
  "inputs": {"ref": "feature/full-history", "closeInterval": "2s", "steps": "all",
             "runs": 1, "machine": "2x", "workers": 8, "pacedLedgers": 10000,
             "name": "cl2s-2x", "publish": "no"},
  "packsPrefix": "s3://stellar-rpc-bench/inputs/synthetic-ledgers/2026-07-18-apply-load-20k",
  "load": {"levelsRps": [250, 500, 1000], "runDuration": "60s", "generateCount": 200, "profile": false},
  "profiles": [
    {"name": "sac-6000", "datasetKind": "synthetic", "networkPassphrase": "<passphrase of the packs>",
     "startChunk": 1, "numChunks": 2, "packs": {"bytes": 20950000000, "seconds": 212.0}}
  ],
  "commit": "91f158b45951545923c0f74d81a6b8be33aed683",
  "runnerCommit": "91f158b45951545923c0f74d81a6b8be33aed683",
  "machine": {"instanceType": "m6id.2xlarge", "cpus": 8, "memoryGiB": 32},
  "blasterCommit": "aadc1a1…",
  "startedAt": "2026-10-01T10:00:03Z",
  "finishedAt": "2026-10-02T04:31:55Z",
  "steps": [
    {"name": "ingest-cold-sac-6000-run1", "kind": "ingest-cold", "profile": "sac-6000",
     "tier": "cold", "run": 1, "loadLevelRps": [], "startChunk": 1, "numChunks": 2,
     "path": "steps/ingest-cold-sac-6000-run1", "status": "ok", "error": ""},
    {"name": "load-hot-sac-6000-run1", "kind": "load", "profile": "sac-6000",
     "tier": "hot", "run": 1, "loadLevelRps": [500], "startChunk": 1, "numChunks": 2,
     "path": "steps/load-hot-sac-6000-run1", "status": "failed",
     "error": "bench-serve exited: 1"}
  ]
}
```

Rules:

- `id` is `<name>-<GitHub run id>`. It is unique for each dispatch.
- `steps[].status` is `pending`, `running`, `ok`, `failed`, `crashed` or
  `skipped`.
- `steps[].loadLevelRps` lists the load levels of a load step. It is empty
  for other kinds.
- Dataset kind, profile and passphrase travel in `campaign.json`. There is
  no `dataset.json` (D20).

### 7.4 Blaster results

`blaster.json` is Blaster's own results file. The box does not convert it.

- Top-level keys: `start`, `end`, `seed`, `duration_seconds`, `aborted`,
  `endpoints`.
- Per endpoint: `total_requests`, `success`, `errors`, `target_rps`,
  `percentiles_ms` (p50, p95, p99, p99.9), `error_types`, and one
  `timeline` entry per window over the whole run. getEvents results are
  also split by archetype (`archetypes`).
- Percentiles include failed and timed-out requests (15-second client
  timeout).
- The text of a JSON-RPC error is the key of `error_types`. Its
  `error_msg` is empty.
- `aborted` marks a run that the kill switch stopped. An aborted run still
  writes `blaster.json` with the windows that completed before the abort.
- The judge reads `aborted`, `success`, `errors` and `error_types` per
  endpoint together with the percentiles.
- `seed` in `blaster.json` must equal `rng_seed` in `blaster.toml`. The
  converter checks it.
- The head is static, so head-poll and near-head requests read the same
  ledgers again and again.

`blaster.json` has no schema version. `load.json` records `blasterCommit`.
The converter supports the fields of that exact commit. It fails with a
named error when the top-level or per-endpoint keys differ. The schema
README of Section 7.2 copies the field list of the pinned commit.

## 8. Benchmarks repository changes

1. Add a workflow that triggers on pushes to `bundle/**`. It converts the
   bundle, runs `make test` and `make smoke`, commits `docs/runs/<id>.json`
   to `main` and deletes the branch.
2. The converter reads the Section 7 layout: `campaign.json`,
   `results.json`, `load.json`, `blaster.json` and the metrics files. It
   reads fields. It does not parse directory names. It takes the dataset
   kind from `campaign.json`. It maps the close interval to a phase and
   applies `docs/targets.json`.
3. A query verdict fails when the error rate exceeds the threshold in
   `docs/targets.json` (a new key), whatever the p99.
4. The site shows the tx-hash index size next to the cold `getTransaction`
   verdict.
5. Remove the runner, its `runner/bootstrap.sh` and `scripts/ingest.sh`
   after stellar-rpc runs its own campaigns.
6. Until the load step works, the site shows no query verdicts (D13).

## 9. What goes away

| Item | Where |
|---|---|
| `bench-query` and its request scheduler, rate rows and accounting file | #12 to #14 |
| The hand-built cold catalog in `bench-query` | #13 |
| The four poll jobs, `.github/actions/bench-poll/action.yml`, `check-result-key.sh`, `gate-relay-state.sh` | #19 |
| The S3 result relay: `perf-eval/relay`, `harness/relay.go`, `upload_result` and `RESULT_KEY` | feature/full-history, #19 |
| The `phase` input and the copied query grid | #15 |
| The `benchmarks_ref` input and the clone of the benchmarks repository on the box | #16, #19 |
| The `run-info.json` sidecar and the `published:` grep | #16 |
| `slack-recap.jq` (it reads converted site JSON, which the box does not have) | #17 |
| `decide-verdict.sh`, `fetch-result-context.sh`, `ingest-results-site.sh`, the notify job and `bench-campaign/README.md` | #20 |
| The run of `ingest.sh` in the stellar-rpc workflow | #20 |
| The old stub inputs `phase`, `ingest`, `query`, `run_name`, `hot_num_ledgers`, `benchmarks_ref` | `bench-campaign.yml` (#922 stub) |
| The runner in the benchmarks repository | benchmarks `runner/` |
| `runner/bootstrap.sh` in the benchmarks repository, after the port | benchmarks `runner/` |

Keep `.github/workflows/ec2-leg.yml` and `perf-eval/gather`.
`load-test-coordinator.yml` uses them.

## 10. Delivery plan

### 10.1 stellar-rpc

Branches are `bench-campaign-v2/NN-<slug>`.

| PR | Branch slug | Content | Depends on | Done when |
|---|---|---|---|---|
| 00 | `spec` | This spec and the decision log. | — | The team agrees. |
| 01 | `readonly-open` | Read-only hot open with events, `catalog.OpenReadOnly`, removal of the ledgers-only comments (Section 6.2). | — | The file-set test passes on a hot chunk directory. `catalog.OpenReadOnly` fails on a missing catalog and writes no file. |
| 02 | `ingest-catalog` | Keep the catalog at `<root>/catalog/rocksdb`, pin the earliest ledger, the frontier chunk in `cold`, the existence check (Section 6.1 item 1). | — | A cold run and a hot run each leave a catalog. `query.NewReadView` succeeds on both catalogs. A second run into the same root fails and writes no file. |
| 03 | `ingest-results` | `results.json` writer (temporary file and rename, `status`), schema README, `--paced-ledgers`. CSV kept (Section 6.1 items 2 to 4, Section 7.2). | — | A cold run and a hot run each write a `results.json` that matches the README. A killed run leaves `status: running`. With `--paced-ledgers 100`, `ingest_total` has 100 samples. |
| 04 | `storage-metrics` | Storage metrics and documentation (Section 6.4). | — | One request records one observation for each store and tier it used. A view with no method name records nothing. A microbenchmark gives the cost of one accumulate-and-observe pair. A test with a slow yield body shows no change in `fullhistory_read_store_seconds`. A cold `getEvents` page records a non-zero open observation. |
| 05 | `bench-serve` | `rpcv2.ServeDataset`, the command, the method filter, the admin registry, the shared `--profile-rates` flag (Sections 6.3, 6.9). | 01, 02 | A test calls each served method over HTTP on a cold and a hot test dataset. Other methods return -32601. The file-set test over the dataset root passes. An empty `--network-passphrase` fails at start. |
| 06 | `campaign-plan` | `cmd/bench-campaign` `plan`, `validate`, `estimate`, `load.toml`, the `campaign.json` schema (Sections 6.5, 7.3). | — | `plan` writes a valid `campaign.json` for each close interval. `validate` rejects each bad input. A test checks the estimate against the formula. |
| 07 | `campaign-run` | `run`: pack fetch with `aws s3 sync`, the ingest steps, the freeze step, the bundle, the status of each step (Sections 4, 6.5, 7.1). | 03, 06 | A local end-to-end test on small packs writes a valid bundle. A killed step leaves a valid `campaign.json`. |
| 08 | `box` | Bootstrap port, user-data stub, box script with an S3 upload per step, push to `bundle/<id>`, Slack payloads from `campaign.json`, `poweroff`, script tests (Section 6.6). | 07, Q2 | The script tests pass. They cover a runner crash, an S3 failure, a push failure and a Slack failure, and check the finish order of Section 4. |
| 09 | `workflow` | Inputs and checks, launch of the box, the reaper, a CI job for the script tests, the stub on `main` (Sections 5, 6.6). | 08 | One campaign with `publish=no` runs on EC2, uploads to S3, posts to Slack and powers off. A manual reaper dispatch terminates a test box with a past deadline. CI runs the script tests. The first run records the size of each hot dataset. |
| 10 | `load-step` | `bench-serve` lifecycle, cache drop, `blaster generate` and `blaster run` flags, the one-hash check, metrics dumps, `load.json`, the Blaster pin and build (Section 6.7). Decide Q3, Q4 and Q8 here. | 05, 07 | One campaign writes `load.json`, `blaster.json` and both metrics dumps for a cold and a hot dataset. A test that kills `bench-serve` gives a failed step with the exit code. |
| 11 | `remove-csv` | Remove the CSV output of `bench-ingest`. | 03, B1 | `bench-ingest` writes no CSV file. The converter tests still pass. |
| 12 | `freeze` | `bench-ingest freeze` (Section 6.8). | 02, 03 | A freeze over a kept hot test dataset writes `results.json`. After it, `query.NewReadView` resolves the frozen chunks to cold. |
| 13 | `bench-live` | `bench-live` (Section 6.10). | 05 | A local test ingests paced ledgers from a small pack tree. At the same time an HTTP client gets each new transaction with `getTransaction`. |

PRs 01, 02, 03, 04 and 06 can go in parallel. Start each PR from
`feature/full-history` at `91f158b` or later. Build and serve a dataset
with the same `ref`: a dataset from a binary before the two-root events
layout (`events/data`, `events/index`) cannot be served by a later binary
(D27).

### 10.2 stellar-rpc-benchmarks

| Change | Content | After |
|---|---|---|
| B1 | The converter reads the Section 7 bundle layout (nested `steps/`, no `-c<chunk>` suffix) and `results.json`. | PR 03 |
| B2 | The `bundle/**` workflow (Section 8 item 1). Land it before the first run with `publish=yes`. | PR 08 |
| B3 | The converter reads `blaster.json`, the metrics and `load.json`. Query verdicts. An error-rate threshold in `targets.json`. | PR 10 |
| B4 | Remove the runner, `runner/bootstrap.sh` and `scripts/ingest.sh`. | PR 08 and PR 09 |

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
8. Keep each PR to about 600 lines of non-test code or fewer. Count them
   with `git diff --stat`, without `*_test.go`, `tests/` and `*.md` (D32).
9. A test runs a script through its command line or through a function.
   It does not slice the script text.

## 12. Open questions

| # | Question | Blocks |
|---|---|---|
| Q1 | Blaster: `main` holds one README that points to `dev`. Another team owns the code on `dev`. `dev` has no LICENSE and no CI. It needs Go 1.25, `jq` and `git`. Do we pin a commit, or talk to the owners first? Ask the owners for a results `schema_version` key or a tag that we can pin. | PR 10 |
| Q2 | Box secrets: where in AWS do the push token and the Slack webhook live (Secrets Manager or Parameter Store)? Who adds them? The instance role `stellar-rpc-ci-load-test` already reads the pack inputs and writes the results bucket. The new grant is read access to the two secrets. | PR 08 |
| Q3 | Blaster's getEvents requests use page limits of 1 to 1,000: about 57% at 100, 23% at 1,000, 18% at 200 and 1.7% at 1. The targets assume 10. `[endpoints.getEvents] limit = 10` forces 10, but it replaces the whole pagination object and removes the archetype mix. Force 10, or keep production shapes and review the targets? | PR 10 |
| Q4 | Blaster ramps in steps of one window and then holds. Without `--ramp-up` it sends at a constant rate. Rates are integers. Do we make one constant-rate run per load level, or one ramp and read one level? Which level does the verdict use? The storage metrics are cumulative, so one run gives one storage share. A share per level needs one run per level. | PR 10 |
| Q5 | Blaster is open loop and measures queue delay. Its percentiles include failed and timed-out samples, so the judge must read the error rate with the p99 (Section 8 item 3). Is a p99 over all samples acceptable, or does the judge need a p99 over successful requests only? | B3 |
| Q6 | Does `bench-serve` need a ready hot chunk above a cold range to get a read view? Answered yes; resolved by D4. | — |
| Q7 | The `sac-6000` packs cover one chunk (generation stopped at ledger 11,008). Regenerate them to two chunks, or run `sac-6000` on one chunk and mark its getEvents verdict? | PR 07 |
| Q8 | Blaster and `bench-serve` share the cores of the box. Pin each one to its own cores, or accept it and record it? | PR 10 |
| Q9 | Do we need a cold dataset with a synthetic 1,000-chunk tx-hash index (hashes only, no ledger packs) to measure `getTransaction` at production scale? | — |
