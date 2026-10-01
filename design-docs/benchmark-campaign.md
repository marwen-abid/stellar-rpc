# Benchmark Campaigns

| | |
|---|---|
| Status | Draft |
| Base | `feature/full-history` at `91f158b` (upstream; the fork was fast-forwarded to it on 2026-09-30) |
| Replaces | [marwen-abid/stellar-rpc#12](https://github.com/marwen-abid/stellar-rpc/pull/12) to [marwen-abid/stellar-rpc#20](https://github.com/marwen-abid/stellar-rpc/pull/20) |
| Decisions | [benchmark-campaign-decisions.md](./benchmark-campaign-decisions.md) |
| Plans | `benchmark-campaign-plan-<id>.md`, one file for each row of Section 10 (`pr02` to `pr13`, `b1` to `b4`) |
| Overview | [RPCv2 Benchmark Campaigns](https://claude.ai/artifact/RWRgf15Ka54ySAzBAHB6k1) (one-page summary) |
| Related | [stellar-rpc-benchmarks](https://github.com/stellar-experimental/stellar-rpc-benchmarks), [stellar-rpc-blaster](https://github.com/stellar/stellar-rpc-blaster) (branch `dev`) |

## 1. Purpose

A benchmark campaign answers these questions about RPCv2:

1. **Ingestion.** Does hot ingestion finish each ledger within the close
   interval? The judge compares the p99 of `driver.ingest_total` over the paced
   ledgers with `ingest_p99_target` (D21). Only this has a target.
2. **Backfill and freeze.** How long do cold ingestion and the freeze take per
   chunk? These have no target.
3. **Queries.** Does each read endpoint meet its p99 latency target under the
   expected traffic? The campaign measures the hot tier and the cold tier
   separately (D14).
4. **Cause.** When a request is slow, how much of its time is storage?

### 1.1 Not measured

- Hot latency while ingestion writes. The hot load step serves a finished hot
  dataset (D3, D14). `bench-live` (Section 6.10) covers this case. The campaign
  uses it only after a new PR that follows PR 10 and PR 13 (D25).
- Discard and prune, the lifecycle stages after the freeze.
- Cold tx-hash lookups against a production-size index. The index of a 2-chunk
  dataset has 2 × 10,000 × (transactions per ledger) keys. We think that it fits
  in the page cache (check in PR 09). Production: 1,000 chunks (Q9).
- A live head. The head does not move during a load step, so near-head requests
  read the same ledgers again and again.



## 2. Who owns what

| Repository | Job | Contains |
|---|---|---|
| `stellar/stellar-rpc` | Measure. | The bench commands, the campaign runner, the workflow, the box bootstrap and the box script. |
| `stellar-experimental/stellar-rpc-benchmarks` | Judge. | The phases and targets (`docs/targets.json`), the converter, the verdicts, the results site. |

The **results bundle** (Section 7) is the only contract between the two
repositories (D9). Every campaign uploads its bundle to S3 (D31). Only a
campaign with `publish=yes` pushes it to the benchmarks repository (D11, D17).

## 3. Words

Use one word for each concept (D19). Do not use `corpus`, `leg`, `cell`, `shed`,
`pacing` or `fixture`. Use "phase" only in a sentence that names the phase
concept of the benchmarks repository. Code identifiers in backticks are not
affected. The defined term "paced ledgers" can use that root.

| Word | Meaning |
|---|---|

| Run N | Repetition N of the steps of one dataset profile (`runs` input). Each run has its own datasets. |
| Runner | The program `bench-campaign`. Its subcommand `bench-campaign run` runs the steps. |
| Blaster run | One `blaster run` process at one load level. A load step makes one or more Blaster runs. |
| Bench command | `bench-ingest cold`, `hot` or `freeze`, `bench-serve` or `bench-live`. |
| Step kind, name, directory | Kind: `ingest-cold`, `ingest-hot`, `freeze` or `load`. Name: `<kind>-<profile>-run<N>`, for example `ingest-cold-sac-6000-run1`; a load step adds the tier: `load-<tier>-<profile>-run<N>`. Directory: `steps/<step name>/` in the bundle. |
| Dataset profile | A synthetic ledger workload, for example `sac-6000`: SAC transfers, 6,000 transactions per ledger. |
| Bench root | The directory of all datasets and packs on one machine: `/mnt/nvme/bench` on a box (`run --root`). |
| Dataset | The directory that one `bench-ingest` step writes: data files and a catalog. Written `<dataset>` below. A test dataset is one that a Go test builds. |
| Clone | A copy of a dataset that `bench-serve` serves (D38). Written `<clone>` below. The runner makes it before `bench-serve` starts and deletes it after. |
| Catalog | A RocksDB database at `<dataset>/catalog/rocksdb`. It records the chunks and their state. The read path uses it to route each request. |
| Chunk | A block of 10,000 ledgers. Chunk `c` starts at ledger `c × 10000 + 2`. |
| Tier | **Hot**: a chunk in its own RocksDB database (a hot chunk database), not yet converted to cold files. **Cold**: a chunk converted to read-only files. |
| Frontier chunk | The empty hot chunk that `bench-ingest cold` adds after its last chunk. The read path needs one ready hot chunk to create a read view (D4). |
| Read view | The view of the stores that one request uses (`query.ReadView`). |
| Paced ledgers | The last N ledgers of a hot dataset. Hot ingestion commits them at the close interval and measures them (D21). |
| Box script | The script on the box that runs the runner and finishes the campaign (Section 6.6). |
| Ceiling | The `shutdown -P` time that the box sets when it starts. |
| Results file | The file that one step writes into its step directory: `results.json` for a bench command, `load.json` for a load step. |
| Results bundle | All step directories of one campaign, plus `campaign.json` and `logs/`. |

## 4. A campaign from start to end

1. A person dispatches the `Bench campaign` workflow (Section 5).
2. The workflow builds `cmd/bench-campaign` at its own commit. `plan` checks the
   inputs and writes `campaign.json`. `estimate` gives the ceiling and the
   `deadline` tag (Section 6.5). The workflow uploads `campaign.json` to S3,
   starts the box, prints the instance id and ends.
3. The box sets its ceiling, gets `campaign.json` from S3, clones stellar-rpc at
   the workflow commit and runs the bootstrap (Section 6.6).
4. The runner builds the binary at `ref`, checks its subcommands and builds
   Blaster (Section 6.5).
5. For each dataset profile, the runner gets the ledger packs into
   `<bench root>/<profile>/packs`. Then, for each run N, it runs these steps in
   this order, with the datasets `<bench root>/<profile>/run<N>/cold` and
   `.../hot`:
   1. `ingest-cold`: `bench-ingest cold` builds a cold dataset of 2 chunks.
   2. `ingest-hot`: `bench-ingest hot` builds a hot dataset of 2 chunks. It
      paces the last `paced_ledgers` ledgers at the close interval.
   3. `load-cold`, then `load-hot`: the load step on each dataset of run N
      (Section 6.7).
   4. `freeze`: `bench-ingest freeze` converts the hot dataset of run N to cold
      files (Section 6.8). It runs last because it changes the hot dataset
      (D33).
6. The `steps` input selects the steps (Section 5). A load step of tier T in run
   N serves the dataset that step `ingest-T` of run N wrote.
7. After each step, the runner rewrites `campaign.json`, and the box script
   uploads the step directory to S3. After the last step of a profile, the
   runner removes its datasets and packs.
8. The box script finishes: S3 upload, push (`publish=yes` only), Slack message,
   box log upload, `poweroff` (Section 6.6, D16). A benchmarks workflow then
   converts the bundle and updates the site (Section 8).

## 5. Workflow inputs

| Input | Values | Default | Meaning |
|---|---|---|---|
| `ref` | branch, tag or commit | `feature/full-history` | The stellar-rpc commit to measure. The runner comes from the workflow's own commit. |
| `close_interval` | choice: `2s`, `1s`, `600ms` | required | Selects the close interval and the dataset profiles. |
| `steps` | choice: `ingest-cold`, `ingest-hot`, `ingest`, `cold`, `hot`, `all` | `all` | The steps of each run. `ingest-cold`: ingest-cold. `ingest-hot`: ingest-hot, freeze. `ingest`: ingest-cold, ingest-hot, freeze. `cold`: ingest-cold, load-cold. `hot`: ingest-hot, load-hot, freeze. `all`: ingest-cold, ingest-hot, load-cold, load-hot, freeze. |
| `runs` | 1 to 5 | 1 | Repetitions of each step. At `2s`, one run of the three profiles has about 17 hours of paced hot ingestion (3 × 10,000 × 2 s). |
| `machine` | choice: `2x`, `8x` | `2x` | `2x` = m6id.2xlarge, `8x` = c6id.8xlarge. |
| `workers` | 1 to 128 | 8 for `2x`, 32 for `8x` | `bench-ingest cold --workers` and `bench-ingest freeze --workers`. |
| `paced_ledgers` | 0 to 20,000 | 10000 | `bench-ingest hot --paced-ledgers`. 0 paces all ledgers. A test campaign can use 100. |
| `name` | letters, digits, `.`, `_`, `-` | `cl<close_interval>-<machine>`, for example `cl2s-2x` | The campaign name. The campaign id is `<name>-<GitHub run id>`. |
| `publish` | choice: `yes`, `no` | `no` | Push the bundle to the benchmarks repository. Test campaigns keep `no`. |

There is no input for the benchmarks repository or the packs prefix
(`load.toml`, Section 6.7). `cmd/bench-campaign` hard-codes the dataset profiles
of each close interval (D10):

| Close interval | Dataset profiles |
|---|---|
| `2s` | `sac-6000`, `custom_token-4000`, `soroswap-1500` |
| `1s` | `sac-5000`, `custom_token-4000`, `soroswap-1500` |
| `600ms` | `sac-6000`, `custom_token-3600`, `soroswap-1800` |

For each profile, the same Go table holds `start_chunk` (1) and the chunk count
(2, D21). It also holds the expected pack bytes and the passphrase of the packs
(Q10). The runner fails a step when the packs do not cover the 2 chunks. The
`sac-6000` packs cover one chunk (Q7).

## 6. Components in stellar-rpc

### 6.1 `bench-ingest` (merged; four changes)

1. **Keep the catalog** (D4, PR 02).
   - Before it opens anything, the command checks that
     `<dataset>/catalog/rocksdb` does not exist. If it exists, the command exits
     with an error and writes no file.
   - Write the catalog at `<dataset>/catalog/rocksdb` and keep it. The dataset
     is `--cold-out-dir` for `cold` and `--hot-dir` for `hot`. Remove
     `--catalog-dir`. Pin the earliest ledger to the first ledger of
     `--start-chunk` with `PinEarliestLedger`.
   - `bench-ingest cold` also creates an empty hot chunk database for the
     frontier chunk (last chunk + 1) under `<dataset>/hot/`. It uses the
     catalog's hot create sequence (`BeginHotCreate`, `hotchunk.Open`,
     `FinishHotCreate`), which marks the chunk `ready`.
2. **Write `results.json`** (Section 7.2, PR 03). Keep the CSV files until the
   converter reads `results.json` (B1). PR 11 then removes them.
3. **Add `--paced-ledgers N` to `bench-ingest hot`** (default 10000; 0 = all).
   - Ledgers before the last N ingest back to back, with no measurement. A value
     above the number of ledgers in the range is an error.
   - `driver.ingest_total`, `driver.pace_lag`, `driver.run_wall` and all `hot.*`
     measurements cover the paced ledgers only.
   - `parameters` records `pacedLedgers` and `unpacedLedgers`.
4. **Campaign flags.** Both ingest steps run with
   `--source pack --pack-dir <bench root>/<profile>/packs`. The hot step always
   passes `--close-interval`.

### 6.2 Dataset clone (D38, PR 05, PR 10)

`bench-serve` serves a clone of a dataset, never the dataset itself (D38). It
opens the clone read-write, as the daemon opens its stores, so the clone can
change. Nothing opens the original, so it does not change.

- **What.** The whole dataset directory: the catalog, the hot chunk databases
  (the frontier chunk included) and the cold files.
- **Where.** `<dataset>-clone`, next to the dataset, for example
  `/mnt/nvme/bench/sac-6000/run1/cold-clone`. A reflink copy needs the same
  filesystem.
- **How.** A copy-on-write copy when the filesystem supports it:
  `cp -r --reflink=always <dataset> <clone>` on XFS made with `reflink=1` (the
  box, PR 08), `cp -c -R <dataset> <clone>` on APFS. It reads no file data and
  takes less than a second. When the reflink copy fails, the runner makes a
  plain copy (`cp -r`) and logs it. A plain copy takes 30 to 90 s on NVMe.
- **When.** The cold load step makes a new clone before each Blaster run,
  because `bench-serve` starts again for each run. The hot load step makes one
  clone before `bench-serve` starts. The runner deletes each clone after the
  `bench-serve` process that served it exits, and at the end of the step.
  `load.json` records the clone time (`cloneSeconds`, Section 7.1).
- **Check.** Before it opens anything, `bench-serve` checks with `os.Stat`
  that `<clone>/catalog/rocksdb` exists. If it does not exist, `bench-serve`
  exits with an error. `catalog.Open` creates a missing catalog, so without
  this check a mistyped path gives an empty catalog.
- **Local run.** `bench-serve` has no clone option. A person who runs
  `bench-serve` by hand makes the clone first, serves it and deletes it:
  ```
  cp -r --reflink=auto <dataset> <dataset>-clone    # Linux; macOS: cp -c -R
  stellar-rpc-v2 bench-serve --dataset <dataset>-clone ...
  rm -rf <dataset>-clone
  ```
  `bench-campaign run` on a laptop makes the clone itself, as on the box.
- A clone does not change the page cache of the original. We think that the
  files of a clone start with no cached pages (check in PR 10). The cold load
  step drops the page cache after it makes the clone (Section 6.7).

### 6.3 `bench-serve` (new, PR 05)

`bench-serve` serves a clone of a dataset (Section 6.2) over JSON-RPC and
does not ingest. `--dataset` names the clone. The wiring is
`rpcv2.ServeDataset(ctx, opts)`. The flags and the cobra command are in
`rpcv2/bench` (D26).

```
stellar-rpc-v2 bench-serve --dataset /mnt/nvme/bench/sac-6000/run1/cold-clone \
  --listen 127.0.0.1:8000 --admin-listen 127.0.0.1:8001 \
  --network-passphrase "<passphrase of the packs>"
```

Start sequence:

1. Fail when `--network-passphrase` is empty (D20).
2. Check with `os.Stat` that `<clone>/catalog/rocksdb` exists; else fail
   (Section 6.2). Build the layout with `geometry.NewLayout(<clone>)`. Open the
   catalog with `catalog.Open`.
3. Build the retention range with size 0 and the chunk of
   `config:earliest_ledger` as the earliest chunk. Get the latest ledger with
   `lastCommittedLedger`, as the daemon does (D3). Call it before item 5: it
   opens the highest ready chunk read-only, and daemon startup also calls it
   before any read-write open. On a cold dataset the empty frontier chunk
   refines nothing, so the latest ledger is the last cold ledger.
4. Assemble the registry with `query.NewRegistry`, not `query.OpenRegistry` (it
   gates `getHealth` on a first commit).
5. Open each `ready` chunk with `hotchunk.OpenReadyWrite`, the open that
   `query.OpenRegistry` uses in the daemon, and call `PublishHandle`.
6. Call `SetLatestLedger` with the latest ledger of item 3 and
   `query.UnknownCloseTime()`.
   Then call `adapters.SeedCloseTimes`. It reads the header of the latest
   ledger and calls `SetLatestLedger(latest, CloseTimeAt(...))` itself.
   `getHealth.oldestLedger` is the first ledger of the earliest chunk. On a cold
   dataset we expect it to equal the first ledger of `start_chunk`: the
   retention size is 0 and `config:earliest_ledger` gives the earliest chunk
   (check in PR 05).
7. Build the configuration with `config.ParseConfig(nil)`. Set
   `[service].endpoint`, `[service].admin_endpoint` and
   `[service.methods.getHealth].max_healthy_ledger_latency` = `1000000h` (D3).
   The value 0 does not turn the check off (`validateService` rejects a limit
   below 1 ms), and test ledgers have close times near 1970.
8. Build the handler with the daemon's code (`newJSONRPCHandler`,
   `handlerParams`) and a non-nil `*feewindow.FeeWindows`. Do not copy the
   method table.
9. After `LimitsByMethod.Apply`, keep only these seven methods (D30):
   `getHealth`, `getNetwork`, `getLatestLedger`, `getLedgers`,
   `getTransactions`, `getTransaction`, `getEvents`. Others return -32601.
10. Register the Go process collectors and the serving collectors on one
    Prometheus registry, as `rpcv2/daemon.go` does. Pass a Daemon type in
    `rpcv2` that wraps `host.MakeNoOpDaemon`: `MetricsRegistry()` returns that
    registry, and `FastCoreClient()` returns the no-op client.
11. Serve `/metrics` and `/debug/pprof` on `--admin-listen`
    (`startAdminServer`). Then write one line `ready` to stdout.

Do not start captive core, ingestion, backfill or the lifecycle loop. If an open
fails, exit non-zero. On SIGTERM, close the listeners, the databases and the
catalog, then exit 0.

### 6.4 Storage metrics (new, also in production; D6, D35, PR 04)

| Metric | Labels | Meaning |
|---|---|---|
| `soroban_rpc_fullhistory_read_store_seconds` (histogram) | `method`, `store` (`ledgers`, `events`, `txhash`), `tier` (`hot`, `cold`) | Time that one request spent in one store and tier. |
| `soroban_rpc_fullhistory_read_open_seconds` (histogram) | `store` | Time to open one cold reader. |

Both use `prometheus.ExponentialBuckets(0.000025, 4, 10)` (25 µs to 6.55 s). The
sink type is `observability.ReadMetrics`.

- `Registry.NewRequestView(method, sink)` creates the read view of a request.
  `NewReadView()` does not change, and its views record nothing (the fee replay
  in `feereplay.go`, `adapters.SeedCloseTimes`).
- The read view adds up the time per store and tier. `ReadView.Release` records
  one observation for each store and tier that the request used.
- `wrapAdapterRequest` gives the method name: the `endpoint` label value of
  `soroban_rpc_json_rpc_request_duration_seconds`, so the series join.
- Time the store's own work only: each iterator step and the reader's first-use
  initialization, not the handler body inside the yield.
- Cold readers open per call, with no reader cache. `ledger.OpenColdReader` and
  `event.OpenColdReader` do no synchronous I/O, so the open cost lands in the
  first read. Measure it at the reader's first blocking call (`init`,
  `waitMeta`, the MPHF load) and observe it once, when the reader closes. For
  `txhash`, time `txhash.OpenColdReader`. Open time also counts in the store
  histogram.
- PR 04 makes `resolveLedgers` return the tier. The signature of
  `ReadView.Events` does not change. Its tier comes from its internal resolve
  step (`resolveTier`).

Storage share per method = Δ `_sum` of the store histogram ÷ Δ `_sum` of
`soroban_rpc_json_rpc_request_duration_seconds`, for the same method, between
the two `/metrics` dumps of a Blaster run.

### 6.5 Campaign runner (new; D15, PR 06, PR 07)

Write `cmd/bench-campaign` in stellar-rpc. It builds with `CGO_ENABLED=0` and
imports no `cmd/stellar-rpc/internal` package. From the benchmarks runner, take
only these parts. From `runner/internal/run`: the lock, the source build and the
step loop. Also the S3 sync (`runner/internal/publish`), the machine data
(`runner/internal/bundle`) and the config validation (`runner/internal/config`).

Subcommands: `plan` (GitHub runner) checks the inputs and writes `campaign.json`
with every step `pending`. `validate <campaign.json>` checks a file.
`estimate <campaign.json>` (GitHub runner) prints the expected time in whole
minutes. `run <campaign.json>` (box or laptop) runs the steps and writes the
bundle next to `campaign.json`. Rules for `run`:

- It builds the binary at `ref` in a second source tree with
  `make build-rpc-v2`, and records `commit` and `runnerCommit`.
- It lists the subcommands with `<bin> __complete ""` and
  `<bin> __complete bench-ingest ""`. An unknown subcommand exits 0 with the
  parent help, so an exit code proves nothing. No `bench-ingest`: exit 2. No
  `bench-serve`: each load step is `skipped`. No `freeze`: each freeze step is
  `skipped` (D33).
- It builds Blaster at the commit that `load.toml` pins (Q1). Blaster needs Go
  1.25 or later, `jq` and `git`.
- It writes each step's output to `logs/<step name>.log` and its own log to
  `logs/runner.log`. After each step, it rewrites `campaign.json` (temporary
  file and rename).
- It knows no targets. The load comes from `load.toml` (Section 6.7, D28).
- Local runs use `--root <bench root>`, `--page-cache drop|keep` and
  `--profile`.

`estimate` adds these terms (keys of `[estimate]` in `load.toml`) and rounds the
sum up to whole minutes:

- `setup_minutes`;
- each profile: `packs.expectedBytes / (fetch_mb_per_s × 10^6)` seconds;
- each `ingest-cold` step: `numChunks × cold_chunk_minutes`;
- each `ingest-hot` step:
  `paced × close interval + unpaced × unpaced_ledger_ms`;
- each load step:
  `serve_start_minutes + generate_minutes + levels × run_duration`, plus
  `levels × serve_start_minutes` for the cold tier;
- each `freeze` step: `numChunks × freeze_chunk_minutes`.

The workflow sets the ceiling to estimate + 60 minutes and the `deadline` tag to
ceiling + 30 minutes.

### 6.6 Workflow and box (D16, D31, PR 08, PR 09)

**Box bootstrap.** Port benchmarks `runner/bootstrap.sh` into
`perf-eval/bench-campaign/box-bootstrap.sh` (`perf-eval` is
`cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval`). It
formats the instance store as XFS with `mkfs.xfs -m reflink=1`, mounts it at
`/mnt/nvme` (bench root `/mnt/nvme/bench`), checks that
`cp --reflink=always` works on it (D38) and runs the fsync probe. It installs
the apt packages, the AWS CLI, and Go and Rust at pinned versions. It sets
`RUSTUP_TOOLCHAIN`, because `rust-toolchain.toml` says `stable`. It runs `scripts/install-zstd.sh` and `scripts/install-rocksdb.sh`
from `ref`, so the native libraries match the grocksdb of `ref`.

**User-data.** Before it starts the box, the workflow uploads `campaign.json` to
`s3://stellar-rpc-bench/results/<id>/campaign.json`. The user-data is a gzipped
stub under 16 KB with no base64 data. It holds the campaign id, the runner
repository and commit, and the ceiling. Its first command after the preamble
sets the ceiling. Then it gets `campaign.json` from S3, clones stellar-rpc at
the workflow commit, and runs the bootstrap and the box script.

**Box script.** It runs `bench-campaign run` and finishes the campaign:

1. After each step, it uploads `campaign.json` and the step directory to
   `s3://stellar-rpc-bench/results/<id>/` (D31).
2. It stops the runner 30 minutes before the ceiling. If the runner exits
   non-zero or dies, it marks the running step `crashed`.
3. It uploads the whole bundle and `logs/`.
4. With `publish=yes`, it creates branch `bundle/<id>` from benchmarks `main`,
   copies the bundle without `logs/` to `bundles/<id>/` and pushes. It tries a
   rejected push up to 5 times.
5. It posts one Slack message from `campaign.json`: the id, the status of each
   step, the S3 prefix, the log keys and the push result. On a push failure, the
   message gives the S3 path.
6. It uploads the box log and runs `poweroff`.

A push failure or a Slack failure does not stop the `poweroff`. If the S3 upload
fails, the box stays up until its ceiling, and Slack says so (D16). The box
reads the push token and the Slack webhook from AWS (Q2), not from the
user-data.

**Reaper.** It terminates boxes tagged `rpc-bench=true` and
`test=stellar-rpc-ci-load-test` whose numeric `deadline` is past. Its schedule
fires only from `main`, so dispatch it by hand until then.

### 6.7 Load step (D22, D28, PR 10)

`cmd/bench-campaign/load.toml` holds the load. `campaign.json` carries a copy
with the same key names (Section 7.3).

| Key | Value |
|---|---|
| `packs_prefix` | `s3://stellar-rpc-bench/inputs/synthetic-ledgers/2026-07-18-apply-load-20k` |
| `run_duration`, `generate_count`, `profile` | `"60s"` (until the Q4 decision), `200`, `false` |
| `[[level]]` `rps`, `rng_seed` | 250 and 1, 500 and 2, 1000 and 3 |
| `[mix]` (percent of the level) | `getTransaction = 60`, `getEvents = 20`, `getTransactions = 15`, `getLedgers = 5` |
| `[estimate]` | `setup_minutes = 60`, `fetch_mb_per_s = 100`, `cold_chunk_minutes = 15`, `unpaced_ledger_ms = 250`, `freeze_chunk_minutes = 15`, `serve_start_minutes = 5`, `generate_minutes = 15` |
| `[blaster]` (PR 10) | `repo = "https://github.com/stellar/stellar-rpc-blaster"`, `commit = "aadc1a17595f418bc92758909aa15a860acea0e8"` |

The packs are at `<packs_prefix>/<profile>/packs-v2/cold`. The runner gets its
`ledgers/` subtree into `<bench root>/<profile>/packs`. Blaster takes an integer
rps for each method, so the runner renders each level with the largest remainder
method (getTransaction, getEvents, getTransactions, getLedgers): 250 → 150, 50,
37, 13; 500 → 300, 100, 75, 25; 1,000 → 600, 200, 150, 50. Each Blaster run uses
one level, its `rng_seed` and its own subdirectory `<level>rps/` (Q4). A load
step runs these steps in this order:

1. Make a clone of the dataset (Section 6.2) and record its time. Start
   `bench-serve` on the clone. Each start appends its stdout and stderr to
   `<step dir>/bench-serve.log`.
2. Poll `getHealth` on `--listen` one time each second until it answers. Stop at
   30 minutes and fail the step.
3. Take `<first>` and `<last>` from `oldestLedger` and `latestLedger` of that
   answer (D22). On a cold dataset `<last>` is the last cold ledger (Section
   6.3).
4. Run this command. Blaster's output goes to `logs/<step name>.log`.
   ```
   blaster generate --rpc-url http://127.0.0.1:8000 --ledger-window <first>,<last> \
     --count <generate_count> --output <step dir>/seed.json
   ```
5. Send one `getTransaction` for a hash from `seed.json`. If the answer is
   `NOT_FOUND`, the passphrase is wrong: fail the step (D20).
6. For each Blaster run:
   1. Cold dataset only: stop `bench-serve` (as in item 7), delete its clone,
      make a new clone and run `sync; echo 3 > /proc/sys/vm/drop_caches` (the
      box runs as root). Then start and wait as in items 1 and 2. The hot
      dataset keeps the cache, the clone and the running `bench-serve`.
   2. Write `<level>rps/blaster.toml`: `rng_seed` and one `[endpoints.<method>]`
      table with `rps` for each method.
   3. Save `/metrics` from `--admin-listen` to `metrics-before.txt`.
   4. Run this command. With no `--ramp-up`, Blaster sends at a constant rate.
      With no `--duration`, it exits 1 with
      `missing required fields in Run mode: duration`.
      ```
      blaster run --rpc-url http://127.0.0.1:8000 --config-path <level>rps/blaster.toml \
        --input-data-path <step dir>/seed.json --duration <run_duration> --error-percent 100 \
        --step-interval 5s --test-output-path <level>rps/blaster.json
      ```
   5. If `bench-serve` exits during the run, stop Blaster and keep the partial
      `blaster.json`. Record `status: failed` and
      `error: "bench-serve exited: <code>"` for the run, mark the remaining runs
      `skipped` and fail the step.
   6. With `profile = true`, save one CPU profile from `/debug/pprof/profile` to
      `<level>rps/cpu.pprof`.
   7. Save `/metrics` again to `metrics-after.txt`.
7. Stop `bench-serve`: SIGTERM, then SIGKILL after 60 seconds. A non-zero exit
   code fails the step. Delete the clone. On a step failure, delete the clone
   too.
8. Write `load.json` (Section 7.1). For each Blaster run it records the page
   cache state, and the GOMAXPROCS and CPU time of both processes. Core pinning
   is Q8.

### 6.8 `bench-ingest freeze` (new; D23, D37, PR 12)

```
stellar-rpc-v2 bench-ingest freeze --dataset /mnt/nvme/bench/sac-6000/run1/hot \
  --workers 8 --out <step dir>
```

- It fails when `<dataset>/catalog/rocksdb` does not exist. It opens the catalog
  read-write: the freeze writes cold files and catalog entries.
- It runs `backfill.RunBackfill` over all consecutive complete ready hot chunks
  from the lowest, including the top chunk when it is complete (D37). Each
  chunk's source is its hot chunk database (`hotchunk.OpenReadyView`), as in the
  lifecycle freeze. A second freeze of a dataset returns an error.
- It uses the driver and the sink of `bench-ingest cold`, so the measurement
  names are the same. It writes `results.json` with `command: freeze`. It does
  not discard the hot chunks.

### 6.9 Profiling (D24)

- Add `--profile-rates <block ns>,<mutex fraction>` to a flag set that
  `bench-ingest`, `bench-serve` and `bench-live` share. It calls
  `runtime.SetBlockProfileRate` and `runtime.SetMutexProfileFraction`. The
  default `0,0` turns both off.
- `bench-ingest` keeps `--cpuprofile` and `--memprofile`. `bench-serve` and
  `bench-live` serve `/debug/pprof` on the admin port.
- With `profile = true`, each bench command step gets
  `--cpuprofile <step dir>/cpu.pprof`, and each Blaster run gets a profile
  (Section 6.7). PR 10 adds this.

### 6.10 `bench-live` (new; D25, PR 13)

`bench-live` runs the daemon body (`startup.go` `run()`) over a paced pack
source and serves JSON-RPC in the same process. PR 13 adds `Core`, `Backend` and
`NetworkPassphrase` to the exported `rpcv2.Options`. It writes its data
directory and runs the lifecycle, so it is a separate command from
`bench-serve`. PR 13 delivers the command and a local test only.

## 7. Results bundle

### 7.1 Layout

```
<campaign-id>/
  campaign.json
  steps/
    ingest-cold-sac-6000-run1/     (also ingest-hot-..., freeze-...)
      results.json, cpu.pprof (profile = true)
    load-cold-sac-6000-run1/       (also load-hot-...)
      load.json, bench-serve.log, seed.json
      500rps/
        blaster.toml, blaster.json, metrics-before.txt, metrics-after.txt
        cpu.pprof                  (profile = true)
  logs/<step name>.log, logs/runner.log, logs/box.log
```

Until PR 11, the ingest step directories also hold the CSV files. The push to
the benchmarks repository leaves out `logs/`.

`load.json` (numbers invented; `runs` has one entry for each Blaster run):

```json
{
  "schemaVersion": 1, "step": "load-cold-sac-6000-run1",
  "blasterCommit": "aadc1a17595f418bc92758909aa15a860acea0e8",
  "startedAt": "2026-10-01T17:27:10Z", "finishedAt": "2026-10-01T17:41:30Z", "status": "ok", "error": "",
  "runs": [
        {"loadLevelRps": 250, "rngSeed": 1, "pageCache": "dropped", "cloneSeconds": 0.4, "serveReadySeconds": 41.2, "benchServeExitCode": 0,
     "cpu": {"benchServeGomaxprocs": 8, "blasterGomaxprocs": 8, "benchServeSeconds": 212.4, "blasterSeconds": 97.1},
     "startedAt": "2026-10-01T17:28:02Z", "finishedAt": "2026-10-01T17:29:03Z", "status": "ok", "error": ""}
  ]
}
```

`runs[].status` is `ok`, `failed` or `skipped`. `pageCache` is `dropped` or
`kept`. `cloneSeconds` is the time to make the clone that the run served
(Section 6.2). `benchServeExitCode` is the exit code of the `bench-serve` process that
served the run. On the hot tier `bench-serve` starts once and all runs share
one process and one clone. So `cloneSeconds`, `serveReadySeconds` and
`benchServeExitCode` have the same value in each run entry.
`<level>rps/blaster.toml` holds the rendered mix of the run.

### 7.2 `results.json` (bench commands)

Example for `bench-ingest hot` (numbers invented):

```json
{
  "schemaVersion": 1, "command": "ingest-hot",
    "binary": {"version": "v2.0.0-dev", "commit": "91f158b…", "buildTimestamp": "2026-10-01T10:31:07"},
  "hostname": "ip-10-0-1-17",
  "startedAt": "2026-10-01T10:45:00Z", "finishedAt": "2026-10-01T17:26:45Z", "status": "ok", "error": "",
  "parameters": {"startChunk": 1, "numChunks": 2, "numLedgers": 0, "closeInterval": "2s",
                 "pacedLedgers": 10000, "unpacedLedgers": 10000, "source": "pack",
                 "flags": {"close-interval": "2s", "paced-ledgers": "10000"}},
  "measurements": [
    {"name": "driver.ingest_total", "kind": "summary", "unit": "ns", "count": 10000, "items": 10000,
     "p50": 410200000, "p90": 612000000, "p99": 880400000, "max": 1203900000, "total": 4151000000000},
    {"name": "driver.peak_rss", "kind": "value", "unit": "bytes", "value": 412000000}
  ]
}
```

In the example, the unpaced ledgers take about 4,100 s (10,000 × 0.41 s). The
paced ledgers take about 20,000 s, so the step takes 6 h 42 m. Rules:

1. Each measurement has a name and a unit, and one unit in each field (D12). All
   measurement values are integers. `unit` is `ns` for durations, `bytes` for
   sizes and `count` for counts. `count` is defined for future rows; no
   `bench-ingest` row uses it today. The converter converts units. The integer
   rule applies to `results.json` measurements only. `campaign.json` and
   `load.json` give seconds as floats (`seconds`, `cloneSeconds`,
   `serveReadySeconds`, `benchServeSeconds`).
2. A measurement name is `<group>.<row>` (D34). The group is the CSV file stem:
   `driver`, `hot`, `ledgers`, `txhash` or `events`. The row is the CSV row name
   without a unit suffix: `driver.ingest_total`, `driver.peak_rss`
   (`unit: bytes`), `ledgers.write`.
3. `kind: summary` has `count`, `items`, `p50`, `p90`, `p99`, `max` and `total`.
   `kind: value` has `value`.
4. `items` is the CSV `n_items` count. The converter derives the hot
   `ledgers_per_s` from `driver.run_wall` `items`.
5. `binary` has `version`, `commit` (the full hash) and `buildTimestamp`.
   `buildTimestamp` is the value of `BUILD_TIMESTAMP` in the `Makefile`:
   `date '+%Y-%m-%dT%H:%M:%S'`, local time of the build machine, with no zone.
   `hostname` is the name of the machine.
6. `command` is the step kind: `ingest-cold`, `ingest-hot` or `freeze`.
7. `status` is `running`, `ok` or `failed`. The command writes the file with
   `status: running` when it starts, and again when it ends. Each write goes to
   a temporary file, then a rename.
8. `driver.ingest_total` is the processing time of one ledger, from extract to
   apply, without the source read and the wait for the due time. The judge
   applies `ingest_p99_target` to it. `driver.pace_lag` is the delay of each
   commit after its due time. The campaign reports it, and no target applies.
9. `parameters` holds `flags` (the raw flag map) and these keys. `ingest-cold`:
   `startChunk`, `numChunks`, `workers`, `source`, `txhashIndexChunks` (the
   chunks that its tx-hash index covers). `ingest-hot`: `startChunk`,
   `numChunks`, `numLedgers`, `closeInterval`, `pacedLedgers`, `unpacedLedgers`,
   `source`. `freeze`: the `ingest-cold` keys and `readyHotChunks`.
10. Document the schema in `cmd/stellar-rpc/internal/rpcv2/bench/README.md` and
    link it from `cmd/bench-campaign/README.md`. Increase `schemaVersion` on
    each incompatible change (D29).

### 7.3 `campaign.json`

`plan` writes `schemaVersion`, `id`, `inputs`, `load`, `profiles` and `steps`,
with every step `pending`. The runner adds `commit`, `runnerCommit`, `machine`,
`blasterCommit`, `startedAt` and `finishedAt`, and updates `steps` and
`profiles[].packs`. Example (numbers invented; one entry of `profiles` and of
`steps`):

```json
{
  "schemaVersion": 1, "id": "cl2s-2x-18234567890",
  "inputs": {"ref": "feature/full-history", "closeInterval": "2s", "steps": "all", "runs": 1,
             "machine": "2x", "workers": 8, "pacedLedgers": 10000, "name": "cl2s-2x", "publish": "no"},
  "load": {"packs_prefix": "s3://stellar-rpc-bench/inputs/…", "run_duration": "60s", "generate_count": 200,
           "profile": false, "level": [{"rps": 250, "rng_seed": 1}, "…"], "mix": {"getTransaction": 60, "…": 0},
           "estimate": {"setup_minutes": 60, "…": 0},
           "blaster": {"repo": "https://github.com/stellar/stellar-rpc-blaster", "commit": "aadc1a1…"}},
    "profiles": [{"name": "sac-6000", "datasetKind": "synthetic", "networkPassphrase": "<passphrase>", "startChunk": 1,
                "numChunks": 2, "packs": {"expectedBytes": 20954563902, "bytes": 20950000000, "seconds": 212.0}}],
  "commit": "91f158b45951545923c0f74d81a6b8be33aed683", "runnerCommit": "91f158b45951545923c0f74d81a6b8be33aed683",
  "machine": {"instanceType": "m6id.2xlarge", "cpus": 8, "memoryGiB": 32}, "blasterCommit": "aadc1a1…",
  "startedAt": "2026-10-01T10:00:03Z", "finishedAt": "",
  "steps": [{"name": "load-cold-sac-6000-run1", "kind": "load", "profile": "sac-6000", "tier": "cold",
             "run": 1, "loadLevelRps": [250, 500, 1000], "startChunk": 1, "numChunks": 2,
             "path": "steps/load-cold-sac-6000-run1", "status": "running", "error": "",
             "startedAt": "2026-10-01T17:27:10Z", "finishedAt": ""}]
}
```

Rules:

- `load` is a copy of `load.toml` with the same key names. `load.blaster` has
  `repo` and `commit` (Section 6.7). The key is absent before PR 10.
- `profiles[].packs.expectedBytes` comes from the Go table. The runner writes
  `bytes` and `seconds` when it has the packs.
- `steps[].status` is `pending`, `running`, `ok`, `failed`, `crashed` or
  `skipped`.
- Only load and freeze steps have `steps[].tier`. A freeze step has `tier: hot`.
- `steps[].loadLevelRps` lists the levels of a load step: all levels of
  `load.toml` until the Q4 decision. It is empty for other kinds.
- The runner writes `steps[].datasetBytes` for each ingest and freeze step
  before it removes the datasets.
- Dataset kind, profile and passphrase travel in `campaign.json`. There is no
  `dataset.json` (D20).

### 7.4 Blaster results

`blaster.json` is Blaster's own output file. The box does not convert it. PR 10
copies the field list of the pinned commit into the schema README (Section 7.2
rule 10). The keys of `percentiles_ms` are `p50.0`, `p95.0`, `p99.0` and
`p99.9`.

- `seed` in `blaster.json` must equal `rng_seed` in `blaster.toml`. The
  converter checks it.
- `blaster.json` has no schema version, so `load.json` records `blasterCommit`.
  The converter supports the fields of that exact commit. It fails with a named
  error when the top-level or per-endpoint keys differ.
- Blaster leaves out the per-endpoint keys `timeline` and `archetypes` when
  they are empty (`writeEndpoint` in
  `cmd/stellar-rpc-blaster/internal/run/metrics/results.go`). The converter's
  key check treats these two keys as optional.
- An aborted run writes `aborted: true`. We think that it keeps the
  `--step-interval` windows that completed before the abort (not verified; check
  in PR 10).

## 8. Benchmarks repository changes

1. A workflow triggers on pushes to `bundle/**`. It converts `bundles/<id>/`,
   runs `make test` and `make smoke`, commits `docs/runs/<id>.json` to `main`
   and deletes the branch. It starts `deploy-pages.yml` with `gh workflow run`,
   because a push with `GITHUB_TOKEN` does not start it.
2. The converter reads the Section 7 layout. It reads fields, not directory
   names. It takes the dataset kind from `campaign.json`, maps the close
   interval to a phase and applies `docs/targets.json`.
3. A query verdict fails when the error rate exceeds a new threshold in
   `docs/targets.json`, whatever the p99.
4. The site shows the tx-hash index size next to the cold `getTransaction`
   verdict.
5. Remove the runner, `runner/bootstrap.sh` and `scripts/ingest.sh` after
   stellar-rpc runs its own campaigns.
6. Until the load step works, the site shows no query verdicts (D13).

## 9. What goes away

| Item | Where | PR |
|---|---|---|
| `perf-eval/relay`, `perf-eval/harness/relay.go` and `relay_test.go`, `unixTime` in `harness/env.go` | feature/full-history | PR 09 |
| The stub inputs `phase`, `ingest`, `query`, `run_name`, `hot_num_ledgers`, `benchmarks_ref` | `bench-campaign.yml` (#922 stub) | PR 09 |
| The runner, `runner/bootstrap.sh` and `scripts/ingest.sh` | benchmarks | B4 |

Keep `.github/workflows/ec2-leg.yml`, `perf-eval/gather`, and `upload_result`
and `RESULT_KEY` in `perf-eval/bootstrap-common.sh`: `ec2-leg.yml` and
`load-test-coordinator.yml` use them. `perf-eval/endpoint-load-test/`
(`run-blaster.sh` and a Go runner) exists on feature/full-history. PR 10 reuses
`run-blaster.sh` if it fits, else removes it. `load-test-coordinator.yml` runs
it too, so a removal must also change that workflow.

## 10. Delivery plan

### 10.1 stellar-rpc

Branches are `bench-campaign-v2/NN-<slug>`. PR 00 stays on `bench-read/00-spec`
until Marwen renames it. D38 dropped PR 01 (read-only opens): `bench-serve`
serves a clone with the daemon's opens, so no storage package changes. The
numbers of the other PRs stay.

| PR | Branch slug | Content | Depends on | Done when |
|---|---|---|---|---|
| 00 | `spec` | This spec and the decision log. | — | The team agrees. |
| 02 | `ingest-catalog` | Keep the catalog at `<dataset>/catalog/rocksdb`, pin the earliest ledger, the frontier chunk in `cold`, the existence check, removal of `--catalog-dir` (Section 6.1 item 1). | — | A cold run and a hot run each leave a catalog. `query.NewReadView` succeeds on both catalogs. A second run into the same dataset fails and writes no file. |
| 03 | `ingest-results` | `results.json` writer (temporary file and rename, `status`), schema README, `--paced-ledgers`. CSV kept. Section 6.1 items 2 to 3, and the README documents item 4 (runner flags built in PR 07). Section 7.2. | — | A cold run and a hot run each write a `results.json` that matches the README. A killed run leaves `status: running`. With `--paced-ledgers 100`, `driver.ingest_total` has 100 samples. |
| 04 | `storage-metrics` | Storage metrics (Section 6.4). Documentation in `docs/MONITORING.md` and the metrics table of `docs/ARCHIVE-NODE-BETA-RUNBOOK.md`. | — | One request records one observation for each store and tier it used. A view with no method name records nothing. A microbenchmark gives the cost of one accumulate-and-observe pair. A test with a slow yield body shows no change in the store histogram. A cold `getEvents` page records a non-zero open observation. |
| 05 | `bench-serve` | `rpcv2.ServeDataset`, the `os.Stat` check of the clone, the command, the method filter, the admin registry, the shared `--profile-rates` flag on `bench-ingest` and `bench-serve` (Sections 6.2, 6.3, 6.9). | 02 | A test calls each served method over HTTP on a clone of a cold and of a hot test dataset. Other methods return -32601. A path without `catalog/rocksdb` fails and creates no file. An empty `--network-passphrase` fails at start. On the cold test dataset, `getHealth.oldestLedger` equals the first ledger of `start_chunk` (check in PR 05, Section 6.3 item 6). |
| 06 | `campaign-plan` | `cmd/bench-campaign` `plan`, `validate`, `estimate`, `load.toml`, the `campaign.json` schema (Sections 6.5, 7.3). | — | `plan` writes a valid `campaign.json` for each close interval. `validate` rejects each bad input. A test checks the estimate against the formula. |
| 07 | `campaign-run` | `run`: the pack fetch, the build of `ref`, the subcommand check, the ingest and freeze steps, the bundle, the step statuses, `steps[].datasetBytes` (Sections 4, 6.5, 7.1). | 03, 06 | A local end-to-end test on a pack tree of 2 chunks writes a valid bundle. A killed step leaves a valid `campaign.json`. |
| 08 | `box` | Bootstrap port, user-data stub, box script with an S3 upload for each step, push to `bundle/<id>`, `slack-payload.sh` and `slack-campaign.jq` rewritten to read `campaign.json`, `poweroff`, script tests (Section 6.6). | 07, Q2 | The script tests pass. They cover a runner crash, an S3 failure, a push failure and a Slack failure, and check the finish order of Section 6.6. |
| 09 | `workflow` | Inputs and checks, upload of `campaign.json`, launch of the box, the reaper, `slack-reaper.jq` and the reaper mode of `slack-payload.sh`, a CI job for the script tests (Python 3.11 or later, bash, jq), the nine inputs on the #922 stub on `main`, removal of the relay (Sections 5, 6.6, 9). | 08 | One campaign with `publish=no` runs on EC2, uploads to S3, posts to Slack and powers off. A manual reaper dispatch terminates a test box with a past deadline. CI runs the script tests. The first campaign records the size of each hot dataset. |
| 10 | `load-step` | The dataset clone and its plain-copy fallback, `bench-serve` lifecycle, cache drop, `blaster generate` and `blaster run` flags, the one-hash check, metrics dumps, `load.json`, the Blaster pin and build, the `profile = true` handling (Sections 6.7, 6.9). Decide Q3, Q4 and Q8 here. | 04, 05, 07 | One campaign writes `load.json`, `blaster.json` and both metrics dumps for a cold and a hot dataset. A test that kills `bench-serve` gives a failed step with the exit code. |
| 11 | `remove-csv` | Remove the CSV output of `bench-ingest`. | 03, B1 | `bench-ingest` writes no CSV file. The converter tests still pass. |
| 12 | `freeze` | `bench-ingest freeze` (Section 6.8). | 02, 03 | A freeze over a kept hot test dataset writes `results.json`. After it, `query.NewReadView` resolves the frozen chunks to cold. |
| 13 | `bench-live` | `bench-live` and the three `rpcv2.Options` fields (Section 6.10). | 05 | A local test ingests paced ledgers from a pack tree of 300 ledgers. At the same time an HTTP client gets each new transaction with `getTransaction`. |

PRs 02, 03, 04 and 06 can go in parallel. Start each PR from
`feature/full-history` at `91f158b` or later. Build and serve a dataset with the
same `ref` (D27).

### 10.2 stellar-rpc-benchmarks

| Change | Content | After |
|---|---|---|
| B1 | Section 8 item 2 for ingest steps: nested `steps/`, `results.json`, D34 names. | PR 03 |
| B2 | Section 8 item 1. Land it before the first campaign with `publish=yes`. | B1, PR 08 |
| B3 | Section 8 items 2 to 4 and 6 for load steps: `load.json`, `blaster.json`, the metrics dumps, the query verdicts. | PR 10 |
| B4 | Section 8 item 5. | PR 08, PR 09, B2 |

## 11. Code rules

1. Each PR runs end to end and contains its own tests.
2. A doc comment says what the code does. It does not describe callers, history
   or rejected options.
3. Validate input one time, at the boundary.
4. Use one cleanup path per function.
5. Put one unit in each field (Section 7.2 rule 1).
6. Do not scrape log lines between programs. Write a file with fields.
7. Use one word for each concept (Section 3).
8. Keep each PR to about 600 added and modified non-test lines or fewer. Pure
   deletions do not count. Count the first column of `git diff --numstat`,
   without `*_test.go`, `tests/` and `*.md` (D32).
9. A test runs a script through its command line or through a function. It does
   not slice the script text.

## 12. Open questions

| # | Question | Blocks |
|---|---|---|
| Q1 | Blaster: `main` holds one README that points to `dev`. Another team owns `dev`, which has no LICENSE and no CI. Do we pin a commit, or talk to the owners first? Can the owners add a results `schema_version` key or a tag that we can pin? | PR 10 |
| Q2 | Box secrets: where in AWS do the push token and the Slack webhook live (Secrets Manager or Parameter Store)? Who adds them? The instance role `stellar-rpc-ci-load-test` needs read access to them. Its grants on the pack inputs and the results bucket are not verified (check in PR 08). | PR 08 |
| Q3 | Blaster's getEvents requests use page limits from 1 to 1,000. The targets assume 10. Do we force a limit of 10, or keep the production shapes and review the targets? | PR 10 |
| Q4 | Do we make one constant-rate Blaster run for each load level, or one ramp and read one level? Which level does the verdict use? | PR 10 |
| Q5 | Blaster percentiles include failed and timed-out samples, so the judge reads the error rate with the p99 (Section 8 item 3). Is a p99 over all samples acceptable, or does the judge need a p99 over successful requests only? | B3 |
| Q7 | The `sac-6000` packs cover one chunk (generation stopped at ledger 11,008). Do we generate them again for two chunks, or run `sac-6000` on one chunk and mark its getEvents verdict? | PR 07 |
| Q8 | Blaster and `bench-serve` share the cores of the box. Do we pin each one to its own cores, or accept it and record it? | PR 10 |
| Q9 | Do we need a cold dataset with a synthetic 1,000-chunk tx-hash index (hashes only, no ledger packs) to measure `getTransaction` at production scale? | — |
| Q10 | No file records the network passphrase of the synthetic packs. PR 06 must get it (from the generator's `METADATA.md` or from the dataset owner) before `plan` works. | PR 06 |

D4 answers Q6.
