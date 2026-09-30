# PR 06: Campaign runner plan, validate and estimate

| | |
|---|---|
| Branch | `bench-campaign-v2/06-campaign-plan` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | None. The pack passphrase must be known (Q10, Section 9). |
| Implements | D10, D15 (plan, validate, estimate part), D21 (dataset shape), D28, D29, D33 (step list); spec 4, 5, 6.5, 7.3 |
| Estimate | About 590 non-test lines: `main.go` 80; `internal/campaign` 190 (types 90, file I/O 40, field checks 60); `internal/plan` 285 (profiles 40, plan 100, `load.toml` parser 65, estimate 30, validate 50); `load.toml` 35; workflow 1 |

## 1. Goal

After this PR, `cmd/bench-campaign` is a Go program with three subcommands.
`plan` checks the workflow inputs and writes `campaign.json` with every step `pending`.
`validate` checks a `campaign.json` file.
`estimate` prints the expected campaign time in minutes.
`cmd/bench-campaign/load.toml` holds the load levels, the traffic mix, the packs prefix and the `[estimate]` table.

## 2. Scope

- New program `cmd/bench-campaign/main.go`: a cobra root with `plan`, `validate` and `estimate`.
- New package `cmd/bench-campaign/internal/campaign`: the `campaign.json` Go types, strict read, atomic write, `Inputs.Validate` and `Load.Validate`.
- New package `cmd/bench-campaign/internal/plan`: the profile table, the step list, `Validate`, the `load.toml` parser, the level rendering and the estimate.
- New file `cmd/bench-campaign/load.toml` (D28), embedded in the binary with `//go:embed`.
- New file `cmd/bench-campaign/README.md`: the subcommands and the `campaign.json` schema.
- Modify `.github/workflows/stellar-rpc.yml`: add `./cmd/bench-campaign/...` to the `go test -race` line.

## 3. Out of scope (boundaries)

- `run`, the pack fetch, the build of `ref`, the step loop and the bundle: PR 07.
- The load step, the Blaster pin and the `[blaster]` table in `load.toml`: PR 10.
- The workflow inputs, the box ceiling and the `deadline` tag: PR 09. PR 09 calls `plan` and `estimate`.
- Targets, verdicts and `docs/targets.json`: the benchmarks repository (D9). D28 rejects a read of `docs/targets.json`.
- The benchmarks runner's `phase` key, `internal/targets`, `queryload.go`, the bench-query steps, the golden preparation and `--resume`: D15 rejects them.
- A `phase` input: D10 rejects it.

## 4. Design

### 4.1 Module layout

- The program lives in the root module `github.com/stellar/stellar-rpc` (`go.mod`, `go 1.26`). The root module already requires `github.com/spf13/cobra v1.7.0` and `github.com/pelletier/go-toml v1.9.5`. No new dependency.
- Today `cmd/` holds only `cmd/stellar-rpc`. Add `cmd/bench-campaign` next to it.
- Go's internal rule forbids an import of `cmd/stellar-rpc/internal/...` from `cmd/bench-campaign`. So the program copies no rpcv2 constant by import. It states the chunk formula (`c × 10000 + 2`, `P/chunk/chunk.go`) in `internal/plan`.
- The program must not use cgo. The GitHub runner builds it with `CGO_ENABLED=0 go build ./cmd/bench-campaign` and no native libraries (PR 09).
- Packages: `internal/campaign` (schema, no policy), `internal/plan` (policy: profiles, steps, load, estimate, `Validate`). `internal/plan` imports `internal/campaign`, and `internal/campaign` imports no other package of the program. `Validate` compares the steps with `plan.Steps`, so it is in `internal/plan`. This prevents an import cycle. PR 07 adds `internal/run`. PR 10 adds `internal/loadstep`.

### 4.2 Take from the benchmarks runner

The benchmarks runner is `runner/` at `3bc7c49` (3,905 non-test lines). This PR takes these parts. The rest is PR 07 (Section 4 of the PR 07 plan).

| Benchmarks runner | This PR | Change |
|---|---|---|
| `internal/config/config.go` `Config.validate`, `reName` (`^[A-Za-z0-9._-]+$`), `validateCloseInterval`, `validateQueryDuration` | `campaign.Inputs.Validate`, `campaign.Load.Validate` | Keep the rule shape and the error text style ("<key> must be ..., got '<v>'"). Drop `repo`, `ingest`, `query`, `phase`, `publish_uri`, `[[dataset]]`. |
| `config.Load` (BurntSushi `toml.DecodeFile`, `md.Undecoded()`) | `plan.ParseLoad` | Use pelletier go-toml v1 `toml.NewDecoder(r).Strict(true)`, as `P/config/config.go` does. Unknown keys fail. |
| `plan.Plan.WriteFile` | `campaign.Write` | Temporary file in the same directory, then `os.Rename`. |

The input checks come from `render-campaign-toml.sh` (#15, branch `origin/bench-campaign/06-notify`, `perf-eval/bench-campaign/`). Port these checks as Go code. Do not port the script.
- `require_uint` (integer, no leading zeros, range). The flags are cobra `Int` flags, so the parser rejects other text.
- `machine` `2x` → `m6id.2xlarge`, workers default 8. `8x` → `c6id.8xlarge`, workers default 32.
- The name: `[A-Za-z0-9._-]+` and no leading `-`. The ref: `[A-Za-z0-9._/-]+` and no leading `-`.
- The URI character set `[A-Za-z0-9._/:=-]` for the packs prefix.
- Drop the `phase` case, `CAPACITY_MINUTES`, the query grid and `BENCHMARKS_REF`.

### 4.3 CLI

```
bench-campaign plan --ref feature/full-history --close-interval 2s --steps all --runs 1 \
  --machine 2x [--workers 8] --paced-ledgers 10000 [--name cl2s-2x] --publish no \
  --run-id 18234567890 [--load load.toml] --out campaign.json
bench-campaign validate campaign.json
bench-campaign estimate campaign.json      # stdout: one integer (minutes, rounded up)
```

- `--name` default: `cl<close interval>-<machine>`, for example `cl2s-2x`, `cl600ms-8x`. The id is `<name>-<run id>` (D16 item 3).
- `--workers` 0 or absent: the machine default.
- `--load` default: the embedded `load.toml`. Tests pass a file.
- `--run-id` must be decimal digits. The workflow passes `${{ github.run_id }}`.
- `estimate` prints the term breakdown to stderr and one integer to stdout. PR 09 sets the ceiling to estimate + 60 and the `deadline` tag to ceiling + 30 (spec 6.5).
- Exit codes: 0 = success; 2 = an input or file check fails. The error goes to stderr as `bench-campaign: <key>: <reason>`.
- The linter bans `fmt.Print*` (`.golangci.yml`, forbidigo). Write with `fmt.Fprintln(cmd.OutOrStdout(), ...)`.

### 4.4 Profile table (`internal/plan/profiles.go`)

```go
type profileSpec struct{ name string; startChunk uint32; numChunks int; packBytes int64 }
func profilesFor(closeInterval string) ([]profileSpec, error)
```

| Close interval | Profiles |
|---|---|
| `2s` | `sac-6000`, `custom_token-4000`, `soroswap-1500` |
| `1s` | `sac-5000`, `custom_token-4000`, `soroswap-1500` |
| `600ms` | `sac-6000`, `custom_token-3600`, `soroswap-1800` |

- `startChunk = 1` and `numChunks = 2` for every profile (D21). The old runner used `chunks = [1]` for every profile (`render-campaign-toml.sh`). Chunk 1 covers ledgers 10,002 to 20,001.
- `packBytes` comes from benchmarks `docs/dataset-sizes.json` `ledger_pack_bytes`: sac-5000 34,927,256,626; sac-6000 20,954,563,902; custom_token-3600 23,170,184,260; custom_token-4000 25,737,649,949; soroswap-1500 10,461,551,928; soroswap-1800 12,715,700,766. These are the `packs/` sizes. The `packs-v2` sizes are not measured. PR 07 records the real bytes.
- One constant `syntheticPassphrase` holds the network passphrase of the packs (D20). Section 9 says how to find it.
- `datasetKind` is `synthetic` for every profile.
- `gochecknoglobals` is on. Return the table from a function. Do not use a package variable.

### 4.5 `load.toml` (D28)

```toml
packs_prefix = "s3://stellar-rpc-bench/inputs/synthetic-ledgers/2026-07-18-apply-load-20k"
run_duration = "60s"
generate_count = 200
profile = false
[[level]]
rps = 250
rng_seed = 1
# levels 500 (seed 2) and 1000 (seed 3) follow the same shape
[mix]   # percent of the level; the sum is 100
getTransaction = 60
getEvents = 20
getTransactions = 15
getLedgers = 5
[estimate]
setup_minutes = 60
fetch_mb_per_s = 100
cold_chunk_minutes = 15
unpaced_ledger_ms = 250
freeze_chunk_minutes = 15
serve_start_minutes = 5
generate_minutes = 15
```

- Packs are at `<packs_prefix>/<profile>/packs-v2/cold` (spec 6.7).
- The `[estimate]` values come from run `phase1-2x-full-history-4b6bc922-20260821T170437Z` on m6id.2xlarge: cold `backfill_wall` for one chunk was 704 s (sac-6000), 271 s and 250 s. Hot `ingest_total` p50 was 160 ms, 105 ms and 99 ms. The values above round up.
- Checks in `campaign.Load.Validate`: at least one level; `rps > 0`; `rng_seed > 0` (Blaster uses the clock when the seed is 0, `configs.go`); levels unique; mix keys in {`getTransaction`, `getEvents`, `getTransactions`, `getLedgers`, `getLatestLedger`, `getHealth`, `getNetwork`} (the D30 methods); mix sum 100; `run_duration` a positive Go duration; `generate_count >= 2`; every `[estimate]` value > 0.
- go-toml v1 matches struct keys without case. Keep the mix as `map[string]int` so the method names keep their case.

Level rendering (`func renderMix(level int, mix map[string]int) map[string]int`): use the largest remainder method. Floor each share. Give the remaining units, one each, to the largest fractional parts. On a tie, the method with the smaller percentage wins. Results: 250 → 150/50/37/13; 500 → 300/100/75/25; 1000 → 600/200/150/50 (spec 6.7 table).

### 4.6 `campaign.json` types (`internal/campaign/campaign.go`)

```go
const SchemaVersion = 1
type Campaign struct { SchemaVersion int; ID string; Inputs Inputs; Load Load; Profiles []Profile
    Commit, RunnerCommit string; Machine *Machine; BlasterCommit, StartedAt, FinishedAt string; Steps []Step }
type Inputs struct { Ref, CloseInterval, Steps string; Runs int; Machine string; Workers, PacedLedgers int; Name, Publish string }
// Load: each field has the same toml and json tag, the load.toml key (packs_prefix, run_duration, ...).
type Load struct { PacksPrefix, RunDuration string; GenerateCount int; Profile bool; Level []Level
    Mix map[string]int; Estimate Estimate }
type Level struct { Rps, RngSeed int }                                  // keys rps, rng_seed
type Profile struct { Name, DatasetKind, NetworkPassphrase string; StartChunk uint32; NumChunks int; Packs Packs }
type Packs struct { ExpectedBytes, Bytes int64; Seconds float64 }
type Step struct { Name, Kind, Profile, Tier string; Run int; LoadLevelRps []int; StartChunk uint32; NumChunks int
    DatasetBytes int64; Path, Status, Error, StartedAt, FinishedAt string }
```

- JSON keys are camelCase, as in spec 7.3, except under `load`. `load` is a copy of `load.toml` with the same key names (D28, spec 7.3): `packs_prefix`, `run_duration`, `generate_count`, `profile`, `level[].rps`, `level[].rng_seed`, `mix`, `estimate`. `plan.ParseLoad` decodes `load.toml` into `campaign.Load`, so one type serves both files.
- The runner fields use `omitempty`. `loadLevelRps` is `[]` (not `null`) for non-load steps. `tier` is omitted for ingest steps. `datasetBytes` is omitted until PR 07 writes it.
- `campaign.json` holds no rendered per-method rps. PR 10 renders each level with `renderMix` and writes it to `<level>rps/blaster.toml`.
- `Read(path)` uses `json.Decoder.DisallowUnknownFields`. It fails when `schemaVersion != 1` (D29).
- `Write(path, c)` writes `<dir>/.campaign.json.tmp-*` with `os.CreateTemp`, syncs it and renames it over `path`.

### 4.7 Step list (`plan.Steps`)

- `steps` input (six choices) → kinds per run: `ingest-cold` → [ingest-cold]; `ingest-hot` → [ingest-hot, freeze]; `ingest` → [ingest-cold, ingest-hot, freeze]; `cold` → [ingest-cold, load-cold]; `hot` → [ingest-hot, load-hot, freeze]; `all` → [ingest-cold, ingest-hot, load-cold, load-hot, freeze] (spec 5, D33).
- The order is profile, then run, then kind (spec 4 item 5).
- Names: `<kind>-<profile>-run<N>`. A load step is `load-<tier>-<profile>-run<N>`. Path: `steps/<name>`.
- `tier`: only load and freeze steps have it (spec 7.3). `cold` for `load-cold`; `hot` for `load-hot` and `freeze`. Ingest steps have no `tier`.
- `loadLevelRps`: all levels of `load.toml` for a load step (Q4 is open; PR 10 can narrow it). Empty for the other kinds.
- `status` is `pending` for every step.
- `pacedLedgers` must not exceed `numChunks × 10000`.

### 4.8 `plan.Validate(c *campaign.Campaign) error` (`internal/plan/validate.go`)

One function. `validate`, `estimate` and PR 07 `run` call it after `campaign.Read`. `plan` calls `Inputs.Validate` on the flags, builds the campaign, then calls `Validate` on the result.
- `inputs`: `c.Inputs.Validate()`, the Section 4.2 rules; runs 1 to 5; workers 1 to 128; pacedLedgers 0 to 20,000; publish `yes|no`.
- `id` = `<inputs.name>-<digits>`.
- `load.packs_prefix`: `s3://` or `file://` (PR 07 tests use `file://`), plus the URI character set.
- `load`: the Section 4.5 rules (`Load.Validate`).
- `profiles`: unique names that match the name rule; `datasetKind` is `synthetic` or `pubnet`; the passphrase is not empty; `numChunks >= 1`. `Validate` does not compare profiles with the Go table. So a test can use its own profile.
- `steps`: the names, in order, equal `Steps(inputs, profiles, load)`; `tier` is set on load and freeze steps only; each status is one of `pending`, `running`, `ok`, `failed`, `crashed`, `skipped`.
- `Validate` returns all failures joined with `errors.Join`, one per line.

### 4.9 Estimate (`plan.Estimate(c) (minutes int, terms []Term)`)

Sum of (spec 6.5):
- `setup_minutes`;
- per profile: `packs.expectedBytes / (fetch_mb_per_s × 10^6)` seconds;
- per `ingest-cold` step: `numChunks × cold_chunk_minutes`;
- per `ingest-hot` step: `paced × closeInterval + unpaced × unpaced_ledger_ms`, with `total = numChunks × 10000`, `paced = total` when `pacedLedgers` is 0, else `pacedLedgers`, and `unpaced = total − paced`;
- per load step: `serve_start_minutes + generate_minutes + levels × run_duration`, plus `levels × serve_start_minutes` for the cold tier (bench-serve restarts per Blaster run, spec 6.7 item 6.1);
- per `freeze` step: `numChunks × freeze_chunk_minutes`.

Round the sum up to whole minutes. Example: `2s`, `all`, runs 1, paced 10,000. The hot terms are 3 × (20,000 s + 10,000 × 0.25 s) = 1,125 minutes.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `cmd/bench-campaign/main.go` | new | cobra root, `plan`, `validate`, `estimate`, `//go:embed load.toml` | 80 |
| `cmd/bench-campaign/load.toml` | new | D28 values | 35 |
| `cmd/bench-campaign/internal/campaign/campaign.go` | new | types, status and kind constants | 90 |
| `cmd/bench-campaign/internal/campaign/file.go` | new | `Read`, `Write` | 40 |
| `cmd/bench-campaign/internal/campaign/validate.go` | new | `Inputs.Validate`, `Load.Validate` | 60 |
| `cmd/bench-campaign/internal/plan/profiles.go` | new | profile table, machines | 40 |
| `cmd/bench-campaign/internal/plan/plan.go` | new | `Plan(inputs, load, runID)`, `Steps` | 100 |
| `cmd/bench-campaign/internal/plan/load.go` | new | `ParseLoad`, `renderMix` | 65 |
| `cmd/bench-campaign/internal/plan/estimate.go` | new | `Estimate`, `Term` | 30 |
| `cmd/bench-campaign/internal/plan/validate.go` | new | `Validate` | 50 |
| `cmd/bench-campaign/README.md` | new | subcommands, schema (not counted) | — |
| `.github/workflows/stellar-rpc.yml` | modify | test `./cmd/bench-campaign/...` | 1 |

## 6. Tests

| Test | Package/file | What it proves | How |
|---|---|---|---|
| `TestPlanEachCloseInterval` | `plan/plan_test.go` | `plan` writes a valid file for `2s`, `1s`, `600ms` | Table test; `Plan` then `Validate`; check profile names and step count (3 × 5 for `all`). |
| `TestStepsPerChoice` | `plan/plan_test.go` | Each of the six `steps` choices gives the spec 5 kinds in order; freeze follows load-hot; only load and freeze steps have `tier` | Compare names and tiers. |
| `TestRenderMix` | `plan/load_test.go` | 250/500/1000 give the spec 6.7 integers; the sum equals the level | Table test. |
| `TestParseLoadRejects` | `plan/load_test.go` | An unknown key, a seed of 0, a mix sum of 99, an unknown method each fail | Strings in the test; `ParseLoad(strings.NewReader(...))`. |
| `TestEmbeddedLoadValid` | `plan/load_test.go` | The embedded `load.toml` parses and validates; `campaign.json.load` has the same key names as `load.toml` | Read `../../load.toml`; decode it and the marshaled `load` into `map[string]any` and compare the key sets. |
| `TestValidateRejects` | `plan/validate_test.go` | Each bad input fails with its key named: runs 0 and 6, workers 129, paced 20,001, name `-x` and `a b`, ref `-r`, machine `4x`, close interval `3s`, steps `x`, publish `maybe`, bad id, bad prefix, a renamed step, a `tier` on an ingest step, a status `done`, an unknown JSON field, `schemaVersion` 2 | One valid campaign from a helper `validCampaign(t)`; each case mutates one field. Each case writes the file to `t.TempDir()`, then calls `campaign.Read` and `Validate`. The unknown field and `schemaVersion` 2 cases edit the JSON bytes and fail in `Read`. |
| `TestWriteReadRoundTrip` | `campaign/file_test.go` | `Write` then `Read` gives the same value; no temporary file remains | `t.TempDir()`. |
| `TestEstimateFormula` | `plan/estimate_test.go` | The estimate equals the Section 4.9 sum | A hand-computed campaign: 1 profile, `all`, paced 100 at `2s`; compare with the formula in the test. |
| `TestMainNoCgo` | `main_test.go` | The program builds with `CGO_ENABLED=0` | `exec.Command("go", "build", "-o", tmp, ".")` with `CGO_ENABLED=0`. |
| `TestCLIPlanValidateEstimate` | `main_test.go` | The three subcommands work through cobra; exit code 2 on a bad input | Call the root command with `SetArgs`; check stdout is one integer for `estimate`. |

There are no rpcv2test helpers here: the program imports no rpcv2 package.

## 7. Done when

- `plan` writes a valid `campaign.json` for each close interval: `go test ./cmd/bench-campaign/internal/plan -run TestPlanEachCloseInterval`.
- `validate` rejects each bad input: `go test ./cmd/bench-campaign/internal/plan -run TestValidateRejects`.
- A test checks the estimate against the formula: `go test ./cmd/bench-campaign/internal/plan -run TestEstimateFormula`.
- The program builds without cgo: `CGO_ENABLED=0 go build ./cmd/bench-campaign`.

## 8. Verification before push

```
go build ./...
go vet ./...
CGO_ENABLED=0 go build -o /tmp/bench-campaign ./cmd/bench-campaign
go test -race ./cmd/bench-campaign/...
make go-check-branch BASE=feature/full-history
/tmp/bench-campaign plan --ref feature/full-history --close-interval 2s --steps all --runs 1 \
  --machine 2x --paced-ledgers 10000 --publish no --run-id 1 --out /tmp/c.json
/tmp/bench-campaign validate /tmp/c.json && /tmp/bench-campaign estimate /tmp/c.json
```

## 9. Risks and open points

- The network passphrase of the packs is not verified. No file in stellar-rpc or the benchmarks repository names it. Read it first from the generator's `METADATA.md` under `gs://rpc-full-history/synthetic-ledgers/2026-07-18-apply-load-20k/<profile>/` (named in `docs/dataset-sizes.json`), or ask the dataset owner. `plan` must fail when the constant is empty. A wrong value makes the one-hash check of PR 10 fail (D20).
- Check that chunk 2 exists for each profile except `sac-6000`: `aws s3 ls <prefix>/<profile>/packs-v2/cold/ledgers/00000/`. The file names are `00000001.pack` and `00000002.pack` (`geometry.LedgerPackPath`, bucket = chunk / 1000). `sac-6000` has one chunk (Q7). The table keeps `numChunks = 2`, so PR 07 fails the `sac-6000` steps until Q7 is decided.
- The estimate is at about 590 lines. If the count goes over 600, split: 06a = schema, `plan`, `validate`, `load.toml`; 06b = `estimate` and the `[estimate]` table.
- `plan` writes every step as `pending` (spec 7.3), because `estimate` and the box need the list before the runner starts.
- `campaign.json` mixes two key styles: snake_case under `load` (the `load.toml` names, D28) and camelCase elsewhere. The key-set test in `TestEmbeddedLoadValid` stops a drift between the two files.
- The `[blaster]` table arrives in PR 10. Until then strict parsing rejects it, and `load` has no `blaster` key.
- The `[estimate]` values are from one 2x run of the old binary. Replace them with the step times from the first PR 09 campaign.
- `gosec` and `funlen` (100 lines, 50 statements) are on. Keep `Validate` split per section.
