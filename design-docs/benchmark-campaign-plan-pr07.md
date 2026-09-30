# PR 07: Campaign runner `run`

| | |
|---|---|
| Branch | `bench-campaign-v2/07-campaign-run` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 03 (`results.json`, `--paced-ledgers`), PR 06 (`internal/campaign`, `internal/plan`). PR 02 for the existence check (the runner removes the dataset directory first, so PR 07 works without it). Q7 decides the `sac-6000` result. D33 is Proposed. |
| Implements | D15 (build, step loop, bundle), D16 item 1 (runner side), D21, D33, D34 (reads `results.json` status only); spec 4, 6.1 item 4, 6.5, 7.1, 7.3 |
| Estimate | About 600 non-test lines: `internal/run/run.go` 170, `steps.go` 95, `source.go` 110, `fetch.go` 90, `machine.go` 55, `lock.go` 35, `main.go` +45 |

## 1. Goal

After this PR, `bench-campaign run <bundle>/campaign.json` runs a campaign on a box or a laptop.
It fetches the packs, builds the stellar-rpc binary at `ref`, runs the ingest and freeze steps, and writes the bundle of spec 7.1.
It rewrites `campaign.json` after each step.
Load steps are marked `skipped` until PR 10 fills the hook.

## 2. Scope

- New `cmd/bench-campaign/internal/run/lock.go`: `AcquireLock(root)`.
- New `internal/run/source.go`: the clone of `ref` in a second tree, the build and the capability check.
- New `internal/run/fetch.go`: the pack fetch per profile and the pack coverage check.
- New `internal/run/run.go` and `steps.go`: the step loop, the step commands, `campaign.json` rewrites, `logs/`, dataset removal.
- New `internal/run/machine.go`: `campaign.json` `machine` from IMDSv2 and `/proc/meminfo`.
- Modify `cmd/bench-campaign/main.go`: add the `run` subcommand. New e2e test in `cmd/stellar-rpc/internal/rpcv2/bench/campaign_e2e_test.go`.

## 3. Out of scope (boundaries)

- The load step body, Blaster, `--page-cache` and `blasterCommit`: PR 10. This PR adds the hook only.
- The S3 upload of the bundle, the push, Slack and `poweroff`: the box script, PR 08 (D16). The runner never uploads.
- `bench-ingest freeze`: PR 12. This PR runs it when the binary has it.
- `--resume`, the golden preparation, `leg.json` sentinels, `tar`, `publish.Run` and `metadata.json`: D15 rejects them.
- The removal of the CSV files: PR 11. The step directories keep them until then (spec 7.1).

## 4. Design

### 4.1 What comes from the benchmarks runner (`runner/` at `3bc7c49`)

| Benchmarks runner | This PR | Change |
|---|---|---|
| `internal/run/lock.go` `AcquireLock` (flock on `<root>/.campaign.lock`) | `run.AcquireLock` | Same. |
| `internal/run/source.go` `EnsureSrc`, `ResolveRef` | `run.ensureSrc`, `run.resolveRef` | Same git commands. The source tree is `<root>/src`. |
| `internal/plan/plan.go` `buildStep` (`git checkout --detach`, `make build-libs`, `make build-rpc-v2`, `mv`) | `run.build` | Same commands. The binary path is `<root>/bin/stellar-rpc-v2-<sha8>`. An existing binary at that path skips the build (`executableExists`). |
| `internal/run/run.go` `Execute`, `firstBadNeed`, `runCommand`, `exitCode`, `removeAll` | `run.Execute` | Keep the sequential walk and the transitive skip. Replace `leg.json` with `campaign.json` statuses. Drop `Resume`, `FailFast`, `PreClean`/`PostClean` lists, `KindTarball`, `KindPublish`. |
| `internal/publish/publish.go` `toolFor` (`aws s3 sync`), `diagnosis`, `exitCode` | `run.fetchPacks` | Reverse direction: S3 to disk. Drop gcloud, `List`, immutability and the `published:` line (spec 11 rule 6). The upload is PR 08. |
| `internal/bundle/metadata.go` `CollectHardware`, `ec2Identity`, `memTotalKB`, `commandOutput` | `run.collectMachine` | Keep IMDSv2 with a 2 s timeout. Write `instanceType`, `cpus`, `memoryGiB`. Drop `metadata.json` and `machine-metadata.txt`. |
| `plan.go` `ingestColdLeg`, `ingestHotLeg` (argv shape) | `steps.go` | New flags: `--num-chunks 2`, `--paced-ledgers`, `--pack-dir <root>/<profile>/packs`. |

Do not take `internal/targets`, `cmd/campaign/queryload.go`, `internal/preflight`, `internal/bundle/bundle.go` (`ValidateResume`) or `resume.go`.

### 4.2 CLI and exit codes

```
bench-campaign run [--root /mnt/nvme/bench] [--repo <git url or path>] [--binary <path>] \
  [--profile] <bundle>/campaign.json
```

- The bundle directory is `dirname(campaign.json)`. The runner writes `steps/<name>/` and `logs/` there. PR 08 relies on this.
- `--root` default `/mnt/nvme/bench` (spec 6.6). Datasets: `<root>/<profile>/run<N>/{cold,hot}`. Packs: `<root>/<profile>/packs`.
- `--repo` default: `git -C <runner checkout> remote get-url origin`. Tests pass a local path.
- `--binary`: use this stellar-rpc binary and skip the clone and the build. For laptops and the e2e test. The runner does not parse `<binary> version` output. It records `commit` from `--binary-commit`, which `--binary` requires.
- `--profile` overrides `load.profile`. With it, bench steps get `--cpuprofile <step dir>/cpu.pprof` (spec 6.9).
- Exit 0: every step `ok` or `skipped`. Exit 1: one or more steps `failed`, or the runner got SIGINT/SIGTERM. Exit 2: an error before the first step (bad `campaign.json`, lock held, clone or build failed, the binary has no `bench-ingest`). On exit 2 every step stays `pending`.

### 4.3 Startup order

1. `campaign.Read` and `campaign.Validate` (PR 06).
2. `AcquireLock(root)`.
3. Open `logs/runner.log`. Tee it with stdout. Use the old `Notef` line format `== [HH:MM:SS] msg`.
4. `runnerCommit`: `debug.ReadBuildInfo()` setting `vcs.revision`. If it is absent (`go run`), use `git -C <runner checkout> rev-parse HEAD`.
5. `ensureSrc(<root>/src, repo)`, `resolveRef(src, inputs.ref)`, `build(src, commit)`. Build output goes to `logs/build.log`. Record `commit` (full hash).
6. `checkBinary(bin)`: see 4.4.
7. `machine = collectMachine()`. Set `startedAt`. `campaign.Write`.

### 4.4 Capability check (D33)

The check must not scrape help text. A probe on the `91f158b` binary showed that `stellar-rpc-v2 bench-ingest freeze --help` and `stellar-rpc-v2 bench-ingest freeze` both exit 0 and print the `bench-ingest` help. So an exit code does not prove that a subcommand exists.

Use cobra's completion protocol (cobra v1.7.0, enabled by default in `cmd/stellar-rpc/rpcv2/main.go`):
- `<bin> __complete ""` lists the root subcommands, one `name<TAB>description` line each, then a `:<directive>` line.
- `<bin> __complete bench-ingest ""` gave `cold` and `hot` at `91f158b`.
- `func subcommands(bin string, path ...string) (map[string]bool, error)` parses the lines before the `:` line.
- No `bench-ingest` → exit 2. No `bench-serve` → mark every load step `skipped` with `error: "binary has no bench-serve"`. No `freeze` under `bench-ingest` → mark every freeze step `skipped` with `error: "binary has no bench-ingest freeze"`.

### 4.5 Pack fetch (`fetch.go`)

Before the first step of each profile:
- `s3://` prefix: `aws s3 sync --no-progress <prefix>/<profile>/packs-v2/cold/ledgers <root>/<profile>/packs`.
- `file://` prefix: copy the same subtree with `filepath.WalkDir` and `io.Copy`. Tests and laptops use it. No S3 in CI.
- Record `profiles[].packs.bytes` (sum of the regular file sizes) and `packs.seconds` (wall time). Write `campaign.json`.
- Coverage: for each chunk `c` in `[startChunk, startChunk+numChunks)` check `<packs>/<c/1000 %05d>/<c %08d>.pack` (`geometry.LedgerPackPath`, `chunk.BucketID`). The old runner passed `--pack-dir=<root>/ledgers` after it synced `packs-v2/cold`; this PR syncs the `ledgers/` subtree so `--pack-dir <root>/<profile>/packs` holds (spec 6.1 item 4).
- A fetch or coverage failure marks the profile's ingest steps `failed` with `error: "packs: <reason>"`. The dependent load and freeze steps are `skipped`. The runner goes on with the next profile. `sac-6000` fails here until Q7 is decided.

### 4.6 Step loop (`run.go`)

```go
type stepFunc func(ctx context.Context, s *campaign.Step, env stepEnv) error
func Execute(ctx context.Context, c *campaign.Campaign, env runEnv) (failed int)
```

- Walk `c.Steps` in order. Skip a step that is already `skipped` (4.4).
- Needs: `load-<tier>` needs `ingest-<tier>` of the same profile and run. `freeze` needs `ingest-hot`. A step whose need is not `ok` becomes `skipped` with `error: "needs <name>: <status>"`. Derive the needs from kind, profile and run. Do not store them.
- Before a step: `status = running`, `startedAt`, `campaign.Write`. After: `ok` or `failed`, `error`, `finishedAt`, `campaign.Write`.
- Each child process: `exec.CommandContext`, stdout and stderr to `logs/<step name>.log`. `cmd.Cancel` sends SIGTERM. `cmd.WaitDelay` is 60 s, then SIGKILL.
- On SIGINT or SIGTERM to the runner: cancel the context, mark the running step `failed` with `error: "runner interrupted"`, write `campaign.json`, exit 1. Leave the later steps `pending`. A SIGKILL to the runner leaves the step `running`; the box marks it `crashed` (D16).
- After the last step of a profile: `os.RemoveAll(<root>/<profile>)` (spec 4 item 9). Before it, set `steps[].datasetBytes` for each ingest and freeze step of the profile. `datasetBytes` is a new field; add it to the PR 06 types and to spec 7.3 (PR 09 needs it: "the first run records the size of each hot dataset").
- At the end: `finishedAt`, `campaign.Write`.

### 4.7 Step commands (`steps.go`)

Ingest cold (spec 6.1 item 4, D21):
```
<bin> bench-ingest cold --source pack --pack-dir <root>/<p>/packs --start-chunk 1 --num-chunks 2 \
  --workers <inputs.workers> --cold-out-dir <root>/<p>/run<N>/cold --out <bundle>/steps/<name>
```
Ingest hot:
```
<bin> bench-ingest hot --source pack --pack-dir <root>/<p>/packs --start-chunk 1 --num-chunks 2 \
  --paced-ledgers <inputs.pacedLedgers> --close-interval <inputs.closeInterval> \
  --hot-dir <root>/<p>/run<N>/hot --out <bundle>/steps/<name>
```
Freeze (spec 6.8, PR 12): `<bin> bench-ingest freeze --dataset <root>/<p>/run<N>/hot --workers <w> --out <step dir>`.

- Remove the target dataset directory before each ingest step. PR 02 makes `bench-ingest` refuse an existing catalog, and there is no resume.
- Success: exit code 0 and `<step dir>/results.json` has `status: ok` (PR 03, spec 7.2 rule 7). Else `failed`. The error is `results.json` `error` when it is set, else `exit status <n>`. The runner reads only `status` and `error`. It does not read measurement names (D34).
- Load hook: `var loadStep stepFunc = skipLoad`. `skipLoad` sets `skipped` and `error: "load step not implemented"`. PR 10 replaces it.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `cmd/bench-campaign/main.go` | modify | `run` subcommand, flags, signal context | 45 |
| `cmd/bench-campaign/internal/run/lock.go` | new | `AcquireLock` | 35 |
| `cmd/bench-campaign/internal/run/source.go` | new | `ensureSrc`, `resolveRef`, `build`, `subcommands` | 110 |
| `cmd/bench-campaign/internal/run/fetch.go` | new | `fetchPacks`, file copy, coverage, `dirBytes` | 90 |
| `cmd/bench-campaign/internal/run/run.go` | new | `Execute`, needs, status writes, signals, cleanup | 170 |
| `cmd/bench-campaign/internal/run/steps.go` | new | argv per kind, `results.json` check, load hook | 95 |
| `cmd/bench-campaign/internal/run/machine.go` | new | `collectMachine` | 55 |
| `cmd/bench-campaign/internal/campaign/campaign.go` | modify | `DatasetBytes` field | 2 |
| `cmd/stellar-rpc/internal/rpcv2/bench/campaign_e2e_test.go` | new | e2e test (test code) | — |
| `cmd/bench-campaign/README.md` | modify | `run`, bundle layout (not counted) | — |

## 6. Tests

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestCampaignEndToEnd` | `cmd/stellar-rpc/internal/rpcv2/bench/campaign_e2e_test.go` | On small packs, `run` writes a valid bundle: `campaign.json` passes `validate`, each ingest step is `ok` with `results.json`, `logs/` holds one log per step, the datasets and packs are gone after the profile | Build the two binaries with `go build` into `t.TempDir()` (`./cmd/stellar-rpc/rpcv2`, `CGO_ENABLED=0 ./cmd/bench-campaign`). Write packs with the existing helper `writeSourcePack(t, <tmp>/prefix/test/packs-v2/cold, chunk, chunk.LedgersPerChunk)` for chunks 0 and 1. Write `campaign.json` with `plan`, then set one profile `test` (startChunk 0, numChunks 2), `packsPrefix` `file://<tmp>/prefix`, `closeInterval` `600ms`, `pacedLedgers` 10. Run `bench-campaign run --root <tmp>/root --binary <bin> --binary-commit test`. Skip under `testing.Short()`. |
| `TestCampaignEndToEndFreeze` | same file | The freeze step is `skipped` when the binary has no `bench-ingest freeze`, else `ok` | Same run; the expected status follows `__complete bench-ingest ""`. |
| `TestKilledStepLeavesValidCampaign` | `run/run_test.go` | A step whose child dies from SIGKILL is `failed`; `campaign.json` stays valid; later steps still run | Fake binary: the test binary re-executes itself (`TestMain` checks `BENCH_CAMPAIGN_FAKE=1`) and acts as `stellar-rpc-v2`: writes `results.json` or kills itself. |
| `TestRunnerKilledMidStep` | `run/run_test.go` | SIGKILL to the runner leaves a readable `campaign.json` with the step `running` (the box marks it `crashed`) | Start the runner as a child process with the fake binary that sleeps; kill it; `campaign.Read` and `Validate`. |
| `TestNeedsSkip` | `run/run_test.go` | A failed `ingest-hot` skips `load-hot` and `freeze` of the same run only | Fake binary fails for one step name. |
| `TestExitCodes` | `run/run_test.go` | 0 when all `ok`/`skipped`, 1 with a failed step, 2 when the lock is held | Fake binary; a second `AcquireLock` in the test. |
| `TestSubcommands` | `run/source_test.go` | `subcommands` parses `__complete` output; a missing `freeze` marks freeze steps `skipped` | Fake binary prints the `91f158b` output. |
| `TestBuildFromLocalRepo` | `run/source_test.go` | `ensureSrc`, `resolveRef` and `build` produce `<root>/bin/stellar-rpc-v2-<sha8>`; a second run skips the build | A `git init` repo in `t.TempDir()` with a `Makefile` whose `build-libs` does nothing and `build-rpc-v2` writes a script. |
| `TestFetchFilePrefix` | `run/fetch_test.go` | A `file://` prefix copies the tree and records bytes; a missing chunk fails the profile with `packs:` | Small files named `00000/00000001.pack`. |
| `TestLock` | `run/lock_test.go` | A second lock on one root fails at once | Port of the benchmarks lock test. |

## 7. Done when

- A local end-to-end test on small packs writes a valid bundle: `go test ./cmd/stellar-rpc/internal/rpcv2/bench -run TestCampaignEndToEnd`.
- A killed step leaves a valid `campaign.json`: `go test ./cmd/bench-campaign/internal/run -run 'TestKilledStepLeavesValidCampaign|TestRunnerKilledMidStep'`.

## 8. Verification before push

```
go build ./...
go vet ./...
go test -race ./cmd/bench-campaign/...
go test -race ./cmd/stellar-rpc/internal/rpcv2/bench/...
make go-check-branch BASE=feature/full-history
```
Run one local campaign on a laptop with `--binary` and a `file://` prefix, and read `campaign.json` and `logs/`.

## 9. Risks and open points

- Size: about 600 lines. If it goes over, split: 07a = `lock.go`, `source.go`, `fetch.go`, `machine.go` and a `run --dry-run` that prints the step commands; 07b = the step loop, the step commands and the e2e test.
- `cmd/bench-campaign` cannot import `cmd/stellar-rpc/internal/...` (Go internal rule), so its tests cannot write packs. The e2e test therefore lives in package `bench`, where `writeSourcePack` is (`bench/bench_test.go`). The brief names `writeLedgerPack`: that helper exists only on `origin/bench-query/03-register-and-tests`. At `91f158b` the helper is `writeSourcePack(t, root, chunkID, numLedgers)`.
- CI: `.github/workflows/stellar-rpc.yml` runs `go test -race -timeout 25m ./cmd/stellar-rpc/...`. The e2e test builds `stellar-rpc-v2` (43 s from a cold cache in this worktree) and ingests 20,000 zero-tx ledgers two times. The existing tests ingest 200 hot ledgers in 0.21 s and 10,000 cold ledgers in 0.28 s. Measure the whole test; keep it under 2 minutes.
- `--binary-commit` is extra CLI surface. The option is to run `<bin> version` and parse it, which spec 11 rule 6 forbids. Keep the flag.
- The pack prefix layout (`packs-v2/cold/ledgers/...`) comes from the old runner and #15. Check it with `aws s3 ls` before the first campaign.
- The fetch needs S3 read on `s3://stellar-rpc-bench/inputs/...` for the instance role `stellar-rpc-ci-load-test` (facts digest A7, not verified).
- New fields `datasetBytes`, `startedAt`, `finishedAt` per step: update spec 7.3 in this PR.
- The e2e runs the freeze step at its skipped state until PR 12 lands. PR 12 must keep `TestCampaignEndToEndFreeze` green.
