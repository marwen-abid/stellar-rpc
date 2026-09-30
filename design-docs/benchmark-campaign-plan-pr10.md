# PR 10: Load step

| | |
|---|---|
| Branch | `bench-campaign-v2/10-load-step` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 04 (storage metrics in the dumps), PR 05 (`bench-serve`), PR 07 (the runner and the load hook). Q1 (Blaster pin). Q3, Q4 and Q8 are decided in this PR. |
| Implements | D2, D14, D20 (one-hash check), D22, D24 (`profile = true`), D28 (`rng_seed`, integer rps); spec 6.5 (Blaster build, `--page-cache`, `--profile`), 6.7, 6.9, 7.1, 7.4, 9 (`run-blaster.sh`) |
| Estimate | About 575 non-test lines: `internal/loadstep/load.go` 150, `serve.go` 120, `blaster.go` 110, `probe.go` 70, `cache_linux.go` + `cache_other.go` 50, `internal/run` hook and flags 45, `load.toml` `[blaster]` 4, `campaign` 20, `load-test-coordinator.yml` 5 (deletions of `endpoint-load-test/` not counted, D32) |

## 1. Goal

After this PR, each load step of a campaign starts `bench-serve` on the dataset of its run, seeds Blaster, runs one Blaster run per load level and stops `bench-serve`.
The step writes `load.json`, `blaster.json`, `blaster.toml`, `seed.json`, `bench-serve.log` and the two metrics dumps per level (spec 7.1).
The runner builds Blaster at a pinned commit.
The PR records the decisions for Q3, Q4 and Q8.

## 2. Scope

- New package `cmd/bench-campaign/internal/loadstep`: the load step sequence and `load.json`.
- `loadstep/serve.go`: the `bench-serve` lifecycle.
- `loadstep/blaster.go`: the Blaster build, `generate`, `run` and `blaster.toml`.
- `loadstep/probe.go`: the one-hash `getTransaction` check, the `/metrics` dumps and the CPU profile.
- `loadstep/cache_linux.go`, `cache_other.go`: the page-cache drop, with the #13 `evict_linux.go` fallback.
- Modify `internal/run`: replace `skipLoad`, add `--page-cache drop|keep` and `--profile`, pass `--cpuprofile` to bench command steps, build Blaster at startup. Modify `load.toml`: add `[blaster]`.
- Decide `perf-eval/endpoint-load-test/` (4.7).

## 3. Out of scope (boundaries)

- `bench-serve` itself, its flags, the method filter and the admin registry: PR 05.
- The storage metric `fullhistory_read_store_seconds`: PR 04. This PR only saves `/metrics`.
- Reading `blaster.json`, the query verdicts and the error-rate threshold: benchmarks B3 (D9).
- Load while ingestion writes: `bench-live`, PR 13 (D25). One load step never spans both tiers (D14).
- Q5 (p99 over all samples): B3.

## 4. Design

### 4.1 Blaster pin and build

- `load.toml` gets `[blaster] repo = "https://github.com/stellar/stellar-rpc-blaster"`, `commit = "aadc1a17595f418bc92758909aa15a860acea0e8"` (branch `dev`, "Add getEvents archetypes to results json (#46)"). The commit is full (spec 7.2 rule 5 style). `campaign.Load` (PR 06) gets `Blaster` with keys `repo` and `commit`, so `campaign.json.load.blaster` is a copy (D28). `Load.Validate` checks a 40-hex commit.
- Build at `run` startup, after the stellar-rpc build: `ensureSrc(<root>/blaster-src, repo)` (PR 07), `git checkout --detach <commit>`, then `make -C <src> build-rpc-blaster`. The Makefile needs `jq` and `git` and stops when `jq` is absent (`Makefile`: "if no jq then no version at compile time"). Blaster `go.mod` says `go 1.25.0`; the box has Go 1.26 (bootstrap), which builds it.
- Fallback when `jq` is missing on a laptop: `go build -o <bin> ./cmd/stellar-rpc-blaster`. The binary then has no version stamp. Log it.
- The binary goes to `<root>/bin/stellar-rpc-blaster-<sha8>`. Record `blasterCommit` in `campaign.json` and in each `load.json`.
- A Blaster build failure marks every load step `failed` with `error: "blaster build: ..."`. Other steps still run.

### 4.2 Step sequence (spec 6.7)

`func Run(ctx context.Context, s *campaign.Step, env Env) error`, with `Env{Bin, Blaster, Dataset, StepDir, Profile campaign.Profile, Load campaign.Load, PageCache string, CPUProfile bool}`. `CPUProfile` is `load.profile` or `run --profile`.

1. `serve.Start`: `<bin> bench-serve --dataset <root>/<p>/run<N>/<tier> --listen 127.0.0.1:8000 --admin-listen 127.0.0.1:8001 --network-passphrase <profile.networkPassphrase>` (spec 6.3). Stdout and stderr go to `<step dir>/bench-serve.log`. Add `--profile-rates` only when PR 05 has the flag and `CPUProfile` is set.
2. `serve.WaitReady`: POST `{"jsonrpc":"2.0","id":1,"method":"getHealth"}` to `http://127.0.0.1:8000` once per second. Ready = HTTP 200 and a JSON-RPC `result`. Stop at 30 minutes and fail the step. Record `serveReadySeconds`. Do not read the `ready` log line (spec 11 rule 6).
3. `blaster generate --rpc-url http://127.0.0.1:8000 --ledger-window <first>,<last> --count <generate_count> --output <step dir>/seed.json`. `<first>` and `<last>` are `oldestLedger` and `latestLedger` of the ready `getHealth` answer (D22, spec 6.7 item 3). The runner does not compute them from chunks. On a cold dataset `<last>` is the last cold ledger. With two values the window is exact (`seed.GetLedgerRange`, case 2). Output to `logs/<step>.log`.
4. One-hash check: read `tx_hashes[0]` from `seed.json` (`seed_data.go` tag `tx_hashes`). POST `getTransaction` with `{"hash": ...}`. Fail the step when `result.status` is `NOT_FOUND`, with `error: "getTransaction NOT_FOUND for a seed hash: check the network passphrase"` (D20). An empty `tx_hashes` also fails.
5. For each level in `step.loadLevelRps` (one Blaster run each), in a subdirectory `<level>rps/`:
   1. Cold tier: `serve.Stop` (as in item 6), `dropPageCache(env.PageCache, dataset)`, `serve.Start`, `serve.WaitReady`. Hot tier: keep the cache and the running process.
   2. Write `blaster.toml`: `rng_seed = <level.rng_seed>` and one `[endpoints.<method>]` table with `rps = <n>` per method of `renderMix(level.rps, load.mix)` (PR 06). Do not write `input_data_path`: `configs.go` fails when both the flag and the key are set.
   3. GET `http://127.0.0.1:8001/metrics` into `metrics-before.txt`.
   4. `blaster run --rpc-url http://127.0.0.1:8000 --config-path <level>rps/blaster.toml --input-data-path <step dir>/seed.json --duration <run_duration> --error-percent 100 --step-interval 5s --test-output-path <level>rps/blaster.json`. No `--ramp-up`. `--step-interval 5s` is the default; pass it so the argv in the step log is complete.
   5. Liveness: a goroutine waits on the `bench-serve` process. When it exits, cancel the Blaster context. Blaster handles SIGINT/SIGTERM (`app.go` `signal.NotifyContext`), so set `cmd.Cancel` to SIGTERM. Keep the partial `blaster.json`. Record `status: failed` and `error: "bench-serve exited: <code>"` for the run, mark the remaining runs `skipped` and fail the step.
   6. With `CPUProfile`: GET `/debug/pprof/profile?seconds=<min(30, duration/2)>` into `cpu.pprof`, started 5 s after Blaster starts.
   7. GET `/metrics` into `metrics-after.txt`.
6. `serve.Stop`: SIGTERM, wait at most 60 s, then SIGKILL. Record the code in `benchServeExitCode` of each run that the process served. A non-zero code fails the step.
7. Write `load.json` with a temporary file and a rename.

`--profile` (spec 6.9): with `CPUProfile`, the ingest and freeze steps of `internal/run` also get `--cpuprofile <step dir>/cpu.pprof`.

### 4.3 Page cache (D22)

- `--page-cache drop|keep` on `run`, default `drop`. `drop` applies to the cold tier only. `runs[].pageCache` is `dropped` or `kept`; the hot tier always records `kept`.
- As root: `unix.Sync()`, then write `3` to `/proc/sys/vm/drop_caches`.
- Not root (a laptop): walk the dataset and call `evictFile` on each regular file. Port `evict_linux.go` (`unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED)`) and `evict_other.go` from branch `origin/bench-query/02-read-path` (D1 allows them). `golang.org/x/sys v0.47.0` is already in `go.mod`. Log the method (`drop_caches` or `fadvise`) to `logs/runner.log`. On a platform without fadvise, fail when `drop` is set.

### 4.4 `load.json` (spec 7.1, D29)

```go
type LoadResult struct { SchemaVersion int; Step, BlasterCommit, StartedAt, FinishedAt, Status, Error string; Runs []LoadRun }
type LoadRun struct { LoadLevelRps, RngSeed int; PageCache string; ServeReadySeconds float64; BenchServeExitCode int
    CPU CPU; StartedAt, FinishedAt, Status, Error string }
type CPU struct { BenchServeGomaxprocs, BlasterGomaxprocs int; BenchServeSeconds, BlasterSeconds float64 }
```
- The JSON keys are those of spec 7.1 and nothing else: `schemaVersion`, `step`, `blasterCommit`, `startedAt`, `finishedAt`, `status`, `error`, `runs[]`. `runs[].status` is `ok`, `failed` or `skipped`.
- `serveReadySeconds` and `benchServeExitCode` belong to the `bench-serve` process that served the run. On the hot tier all runs share one process, so they share both values.
- `cpu.blasterSeconds`: `ProcessState.UserTime() + SystemTime()` after `blaster run` exits.
- `cpu.benchServeSeconds`: Δ `process_cpu_seconds_total` between `metrics-before.txt` and `metrics-after.txt`. `host/metrics.go` registers `collectors.NewProcessCollector`; PR 05 registers the process collectors on its registry (spec 6.3 item 10).
- `cpu.benchServeGomaxprocs`: `go_sched_gomaxprocs_threads` from `metrics-before.txt` (client_golang v1.23.2 Go collector; verify the name). `cpu.blasterGomaxprocs`: the `GOMAXPROCS` environment value, else `runtime.NumCPU()` of the runner.
- The rendered mix of a run is in `<level>rps/blaster.toml` (spec 7.1). The Blaster argv goes to `logs/<step name>.log`.

### 4.5 Blaster facts checked at `aadc1a1`

- `run` flags (`internal/cli/cli.go`): `--config-path`, `--test-output-path` (default `./output`), `--input-data-path`, `--duration` (default 0), `--ramp-up` (0), `--step-interval` (5s), `--serial`, `--cooloff`, `--error-percent` (50), plus the common `--rpc-url`. `generate`: `--output` (`./output/seed.json`), `--ledger-window START[,END]`, `--count` (5000, `Uint32`).
- A `--test-output-path` that ends in `.json` is used as the file (`app.go` `setOutput`). The directory is created.
- Config keys (`internal/config/configs.go`): `rng_seed` (`uint64`; 0 = clock seed), `input_data_path`, `[endpoints.<m>]` `rps` (`int`), `start_rps`, `limit`. `config.example.toml` says `data_path`, but the struct tag is `input_data_path`. Blaster decodes with go-toml v1 `Unmarshal`, which ignores unknown keys, so a `data_path` key is dropped with no error. The runner uses the flag only.
- `--step-interval` is checked only when `--ramp-up > 0`. Keep 5s: `aggregator.go` uses `max(5, settings.StepInterval)`, so 0 gives one window per 5 ns.
- Without `--duration` (default 0), `App.RunApp` returns `missing required fields in Run mode: duration`, and `main.go` exits 1 (spec 6.7 item 6.4). The runner always passes `--duration`.
- `run` calls `getNetwork` at start and `getHealth` as a preflight (`latestLedger != 0`, seed `ledger_range.first >= oldestLedger`). The window of 4.2 item 3 starts at `oldestLedger` of `getHealth`, so the preflight holds.

### 4.6 Open questions for this PR (list the facts; decide with Marwen; record each decision in the log)

Q3, getEvents page limit. Facts: Blaster's archetype limits give about 57% at 100, 23% at 1,000 (deep-pager), 18% at 200 and 1.7% at 1 (`events_archetypes.go`). The targets assume 10 (`targets.json` `events_page_limit`). `[endpoints.getEvents] limit = 10` forces 10 on every request and replaces the whole pagination object, so the archetype mix goes.
- Option A: force `limit = 10`. The requests match the targets; the shapes do not match production.
- Option B: keep the Blaster shapes; B3 reviews the events target.
- Either option is one optional `limit` key per method in `load.toml` `[limit]`, rendered into `blaster.toml`. About 10 lines.

Q4, runs per load step. Facts: without `--ramp-up`, Blaster sends a constant rate. A ramp steps each `--step-interval` window and holds; a level read from a ramp gets 5 s windows bucketed by response arrival, and whole-run `percentiles_ms` mix levels. The storage metrics are cumulative, so one storage share needs one run.
- Option A: one constant-rate run per level (3 runs; the plan default). Load time per step = 3 × duration + restarts.
- Option B: one ramp run; the verdict reads one window. One share for the whole ramp.
- Option C: one constant-rate run at one level (for example 500). Set `loadLevelRps` to one value in PR 06 `plan`.
- Also decide the duration. 60 s gives about 6,000 getEvents samples at 500 rps. Section 4.9 of the PR 06 plan puts the duration in the estimate.

Q8, core sharing. Facts: Blaster and `bench-serve` share the box. 2x = m6id.2xlarge, 8 vCPU. Blaster can start up to 24,576 workers (`MaxWorkers`).
- Option A: accept and record GOMAXPROCS and CPU time (D22 already does this).
- Option B: pin with `taskset -c` (for example 0-5 for `bench-serve`, 6-7 for Blaster on 2x). Record the CPU sets in `load.json` (a spec 7.1 change and a `schemaVersion` question, D29). About 20 lines. `taskset` is in util-linux on Ubuntu 24.04.

### 4.7 `perf-eval/endpoint-load-test/` (spec 9)

- At `91f158b` it holds `run-blaster.sh` (7 lines) and `runner/` (351 non-test lines). `ec2-leg.yml` runs `run-blaster.sh` on a box: it sources `bootstrap-common.sh` and runs `runner/`, which clones Blaster at `dev` HEAD and blasts a remote `TARGET_RPC`. `load-test-coordinator.yml` job `blaster` runs it (line 140), and job `report` needs `blaster` (line 149).
- It does not fit the load step: no pin, no seed, one remote target, no `bench-serve`. Default: remove the directory, the `blaster` job, the `blaster` entry of `report.needs` and the `Endpoint load test` entry of the list at line 83.
- Ask the owner of `load-test-coordinator.yml` first. If the job must stay, keep the files and record it in the decision log.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `cmd/bench-campaign/internal/loadstep/load.go` | new | `Run`, `LoadResult`, `load.json` write | 150 |
| `cmd/bench-campaign/internal/loadstep/serve.go` | new | `Start`, `WaitReady`, `Stop`, liveness | 120 |
| `cmd/bench-campaign/internal/loadstep/blaster.go` | new | `BuildBlaster`, `generate`, `runBlaster`, `writeBlasterTOML` | 110 |
| `cmd/bench-campaign/internal/loadstep/probe.go` | new | JSON-RPC call, one-hash check, `/metrics`, pprof, metric parse | 70 |
| `cmd/bench-campaign/internal/loadstep/cache_linux.go`, `cache_other.go` | new | `dropPageCache`, `evictFile` | 50 |
| `cmd/bench-campaign/internal/run/run.go`, `steps.go`, `main.go` | modify | hook, `--page-cache`, `--profile`, `--cpuprofile`, Blaster build | 45 |
| `cmd/bench-campaign/internal/campaign/campaign.go`, `validate.go` | modify | `Blaster` in `Load` and its check | 20 |
| `cmd/bench-campaign/load.toml` | modify | `[blaster]` | 4 |
| `perf-eval/endpoint-load-test/`, `.github/workflows/load-test-coordinator.yml` | delete / modify | 4.7 default | 5 |

## 6. Tests

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestLoadStepFakeServe` | `loadstep/load_test.go` | The sequence writes `load.json` (spec 7.1 keys only), `blaster.toml`, both dumps per level and keeps the step order; `--ledger-window` equals the fake `getHealth` `oldestLedger,latestLedger` | Fake `bench-serve`: the test binary re-executes itself (`TestMain`, env `LOADSTEP_FAKE=serve`) and serves `getHealth`, `getTransaction`, `/metrics`. Fake Blaster (`LOADSTEP_FAKE=blaster`) writes `seed.json` and `blaster.json`. |
| `TestServeKilledFailsStep` | `loadstep/load_test.go` | A killed `bench-serve` gives a failed run with `error: "bench-serve exited: ..."` and the exit code, the partial `blaster.json`, `skipped` remaining runs and a failed step | The fake serve kills itself 1 s into the Blaster run. |
| `TestNotFoundFailsStep` | `loadstep/load_test.go` | `NOT_FOUND` on the seed hash fails the step before any Blaster run | The fake serve answers `NOT_FOUND`. |
| `TestReadyTimeout` | `loadstep/serve_test.go` | No answer until the limit fails the step | Limit injected as 2 s. |
| `TestBlasterTOML` | `loadstep/blaster_test.go` | The TOML decodes into Blaster's shape: `rng_seed`, integer `rps`, no `input_data_path` | Decode with go-toml v1 into a local struct with the `configs.go` tags. |
| `TestColdRestartsServe` | `loadstep/load_test.go` | Cold: one serve start per level plus the first, `pageCache: dropped`; hot: one start, `kept`, one exit code on all runs | Count fake-serve starts. |
| `TestProfileFlag` | `run/steps_test.go` | `--profile` adds `--cpuprofile <step dir>/cpu.pprof` to ingest and freeze argv | Compare argv. |
| `TestLoadStepRealServe` | `cmd/stellar-rpc/internal/rpcv2/bench/campaign_e2e_test.go` | On the PR 07 test dataset, the real `bench-serve` becomes ready, and the one-hash check finds a seeded hash | Extend `TestCampaignEndToEnd` (PR 07) with `steps=all`; use the fake Blaster from a `BLASTER` override path. Skip under `testing.Short()`. |

## 7. Done when

- One campaign writes `load.json`, `blaster.json` and both metrics dumps for a cold and a hot dataset: a PR 09 campaign with `publish=no`, then `aws s3 ls s3://stellar-rpc-bench/results/<id>/steps/load-cold-<p>-run1/500rps/` lists the files. Locally: `go test ./cmd/stellar-rpc/internal/rpcv2/bench -run TestLoadStepRealServe`.
- A test that kills `bench-serve` gives a failed step with the exit code: `go test ./cmd/bench-campaign/internal/loadstep -run TestServeKilledFailsStep`.
- The decision log has entries for Q3, Q4 and Q8, and spec 12 marks them decided.

## 8. Verification before push

```
go build ./...
go vet ./...
CGO_ENABLED=0 go build ./cmd/bench-campaign
go test -race ./cmd/bench-campaign/...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
make go-check-branch BASE=feature/full-history
git -C <blaster clone> checkout aadc1a1 && make -C <blaster clone> build-rpc-blaster
```
Run one laptop load step against a PR 07 test dataset with the real Blaster, and read `blaster.json` `seed` = `rng_seed`.

## 9. Risks and open points

- Q1 blocks the pin. Blaster `dev` has no LICENSE and no CI. Check that the box can clone `stellar/stellar-rpc-blaster` without a token. A private repository needs a token from Q2.
- `blaster run` replaces 12% of getTransaction hashes with one of 8 hashes that never land (`util.PrTxNotFound` = 0.12, `parameters/endpoints.go`). These requests are expected to return `NOT_FOUND`. The one-hash check reads `seed.json`, which holds only hashes from the dataset. Not verified on a synthetic dataset. B3 must not read those `NOT_FOUND` answers as a passphrase defect.
- `generate --count 200` with 6,000 transactions per ledger pages `getTransactions` twice per sampled range at limit 50 (`util.DefaultTxPageLimit`; not measured). Measure the generate time in the first campaign and update `generate_minutes`.
- `process_cpu_seconds_total` and `go_sched_gomaxprocs_threads` depend on PR 05 registering the Go and process collectors. If PR 05 does not, read `/proc/<pid>/stat` fields 14 and 15 instead (about 15 more lines).
- Estimate is about 560 lines. Q8 option B adds about 20. If the total goes over 600, move the Blaster build (4.1) and the page-cache code (4.3) into 10a, and the sequence into 10b.
