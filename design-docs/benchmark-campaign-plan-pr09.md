# PR 09: Campaign workflow, reaper and script CI

| | |
|---|---|
| Branch | `bench-campaign-v2/09-workflow` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later). One extra small PR against stellar/stellar-rpc `main` for the stub (4.6). |
| Depends on | PR 06 (`plan`, `validate`, `estimate`), PR 07 (`run`, `steps[].datasetBytes`), PR 08 (box scripts), Q2 for the first EC2 run |
| Implements | D10, D16 (5), D17; spec Sections 5, 6.6, 9 |
| Estimate | about 400 non-test lines added: `bench-campaign.yml` 120, `plan-campaign.sh` 55, `launch-box.sh` 50, `bench-reaper.yml` 50, `reap-boxes.sh` 40, `slack-reaper.jq` 29 + reaper mode in `slack-payload.sh` 12, `bench-scripts.yml` 45. Removed: about 140 (relay). |

`PE` = `cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval`; `BC` = `PE/bench-campaign`.

## 1. Goal

After this PR, a person dispatches `Bench campaign` with nine inputs. The
workflow runs `bench-campaign plan`, `validate` and `estimate` on the GitHub
runner, starts one tagged box with the PR 08 user-data, prints the instance
id and ends. The reaper terminates boxes past their `deadline` tag. A CI job
runs the script tests. The S3 relay code is gone.

## 2. Scope

- Rewrite `.github/workflows/bench-campaign.yml` (today the #922 stub).
- Add `BC/plan-campaign.sh` (plan, validate, estimate, ceiling) and
  `BC/launch-box.sh` (from #19).
- Add `.github/workflows/bench-reaper.yml` (from #18) with its step body in
  `BC/reap-boxes.sh`; add `BC/slack-reaper.jq` and the reaper mode of
  `BC/slack-payload.sh` (from #17).
- Add `.github/workflows/bench-scripts.yml`: Python tests, shellcheck,
  actionlint.
- Remove `PE/relay/main.go`, `PE/harness/relay.go`, `PE/harness/relay_test.go`
  and the relay parts of `PE/harness/env.go` and `env_test.go`.

## 3. Out of scope (boundaries)

- Box scripts and Slack campaign card: PR 08.
- Input checks inside Go: PR 06 (`bench-campaign plan` exits 2 and prints
  `bench-campaign: <input>: <reason>`).
- `ec2-leg.yml`, `load-test-coordinator.yml`, `PE/gather`, `harness/poller.go`,
  `harness/gather.go`, `bootstrap-common.sh` `upload_result`/`RESULT_KEY`:
  kept. `ec2-leg.yml` needs them (4.7).
- The `bundle/**` workflow: B2. Load-step inputs: none (D15; `load.toml`).

## 4. Design

### 4.1 Inputs (spec Section 5)

| Input | Type | Default | Passed as |
|---|---|---|---|
| `ref` | string | `feature/full-history` | `--ref` |
| `close_interval` | choice `2s`, `1s`, `600ms` | required | `--close-interval` |
| `steps` | choice `ingest-cold`, `ingest-hot`, `ingest`, `cold`, `all` | `all` | `--steps` |
| `runs` | string | `1` | `--runs` (1 to 5) |
| `machine` | choice `2x`, `8x` | `2x` | `--machine` |
| `workers` | string | empty | `--workers` (empty = 8 or 32) |
| `paced_ledgers` | string | `10000` | `--paced-ledgers` (0 to 20000) |
| `name` | string | empty | `--name` (empty = `cl<interval>-<machine>`) |
| `publish` | choice `yes`, `no` | `no` | `--publish` |

Every input reaches a script through `env:`. No `${{ inputs.* }}` appears in
a `run:` body (shell injection). `plan` is the only validator (spec Section
11 rule 3).

### 4.2 `bench-campaign.yml`

One job `launch`, `runs-on: ubuntu-latest`, `timeout-minutes: 20`,
`permissions: {id-token: write, contents: read}`. No other secret than
`AWS_GHA_ROLE_ARN`. `BENCHMARKS_PUSH_TOKEN` and `SLACK_BENCH_WEBHOOK_URL` are
not read here. Steps:

1. `actions/checkout@v4` at `github.sha`.
2. `actions/setup-go@v5` with `go-version-file: go.mod`, then
   `CGO_ENABLED=0 go build -o "$RUNNER_TEMP/bench-campaign" ./cmd/bench-campaign`.
   PR 06 keeps the command free of cgo and of `cmd/stellar-rpc/internal`
   imports (agreed with the PR 06 author), so no native libs are needed.
3. `BC/plan-campaign.sh` (id `plan`).
4. `aws-actions/configure-aws-credentials@v4`, `role-duration-seconds: 900`.
5. `BC/render-user-data.sh` with `CAMPAIGN_ID`, `CAMPAIGN_JSON`,
   `RUNNER_REPO=${{ github.repository }}`, `RUNNER_COMMIT=${{ github.sha }}`,
   `CEILING_MINUTES`, `RUN_URL`.
6. `BC/launch-box.sh` (id `launch`).
7. Summary: id, instance id, ceiling, deadline (UTC), S3 prefix
   `s3://stellar-rpc-bench/results/<id>/`, `aws ssm start-session --target
   <instance id>`, box log key `logs/box.log`.

No poll job, no notify job, no cleanup job. No `concurrency` group: each
dispatch has its own id and box.

### 4.3 `plan-campaign.sh`

Env in: the nine inputs as `REF`, `CLOSE_INTERVAL`, `STEPS`, `RUNS`,
`MACHINE`, `WORKERS`, `PACED_LEDGERS`, `NAME`, `PUBLISH`; `RUN_ID`;
`BENCH_CAMPAIGN` (binary path); `OUT_DIR`. Steps:

```
"$BENCH_CAMPAIGN" plan --ref "$REF" ... --run-id "$RUN_ID" --out "$OUT_DIR/campaign.json"
"$BENCH_CAMPAIGN" validate "$OUT_DIR/campaign.json"
EST=$("$BENCH_CAMPAIGN" estimate "$OUT_DIR/campaign.json")   # integer minutes
```

It checks that `EST` is a positive integer. `CEILING_MINUTES = EST + 60`
(spec Section 6.5). It appends `id` (`jq -r .id`), `campaign_json`,
`estimate_minutes`, `ceiling_minutes` and `instance_type` (`m6id.2xlarge` or
`c6id.8xlarge`) to `GITHUB_OUTPUT`, and a table to `GITHUB_STEP_SUMMARY`. On
a `plan` failure it prints each stderr line as `::error::<line>`, writes no
output and exits 1. It passes `--workers` only when `WORKERS` is not empty.

### 4.4 `launch-box.sh` (from #19, 45 lines)

Keep: AMI `ami-052355af2a014bd2c` (Ubuntu 24.04), instance profile
`stellar-rpc-ci-load-test`, `--instance-initiated-shutdown-behavior
terminate`, 50 GB gp3 root, tags on instance and volume. Change:
`SELF_TERMINATE_MINUTES` becomes `CEILING_MINUTES`;
`deadline = now + (CEILING_MINUTES + 30) * 60` (ceiling + 30 minutes, spec
Section 6.5); tags `test=stellar-rpc-ci-load-test`, `rpc-bench=true`,
`run-id=<GitHub run id>`, `campaign-id=<id>`, `deadline=<epoch>`,
`Name=bench-campaign-<id>`. Output `instance_id` and `deadline_epoch` to
`GITHUB_OUTPUT`. The `deadline` is computed at launch, so queue delay cannot
shorten it.

### 4.5 Reaper

`bench-reaper.yml` keeps the #18 triggers (`schedule: cron '17,47 * * * *'`,
`workflow_dispatch`), `concurrency: bench-reaper`, OIDC, `timeout-minutes:
10`. Move the `Terminate boxes past their deadline` body to
`BC/reap-boxes.sh` unchanged in logic: `aws ec2 describe-instances` with
filters `tag:rpc-bench=true`, `tag:test=stellar-rpc-ci-load-test`, state
`pending,running`; select a numeric `deadline` below `date +%s`; `aws ec2
terminate-instances`; outputs `untagged`, `reaped_json`, `terminated`. The
alert step runs `post-slack.sh` with `MODE=reaper`. Update the header
comment: the backstops are the box ceiling and `poweroff`; there is no
cleanup job. The schedule fires only from the default branch (4.6).

### 4.6 The stub on `main`

Verified: `main` (`origin/main` `1ec5abe`, a copy of upstream `main`) holds
the #922 stub `.github/workflows/bench-campaign.yml` with the ten old inputs
(`phase`, `ingest`, `query`, `runs`, `workers`, `machine`, `ref`,
`hot_num_ledgers`, `run_name`, `benchmarks_ref`). GitHub lists the workflow
because the file exists. The form takes its inputs from the selected ref, so
a dispatch on `feature/full-history` shows the nine new inputs. Open one PR
against stellar/stellar-rpc `main` that replaces the stub's inputs with the
nine of 4.1 and keeps the failing `stub` job. `bench-reaper.yml` does not go
to `main`: its step needs `BC/` scripts, which reach `main` only when
`feature/full-history` merges. Until then, dispatch the reaper by hand.

### 4.7 What goes away (spec Section 9)

On `feature/full-history` at `91f158b` (verified):

- `PE/relay/main.go` (8 lines), `PE/harness/relay.go` (120),
  `PE/harness/relay_test.go` (342, test).
- `PE/harness/env.go`: `unixTime` (only `relayConfig.Deadline` uses it) and
  the Relay words in the `PollerConfig` comment. `env_test.go`: the
  `relayConfig` cases and `setRelayEnv`.
- `PE/harness/harness.go`, `poller.go`: remove the Relay lines of the doc
  comments.
- `.github/workflows/bench-campaign.yml`: the stub body and its ten inputs.

Never ported from #19 and #20, so nothing to delete: `.github/actions/bench-poll/action.yml`,
`check-result-key.sh`, `gate-relay-state.sh`, `decide-verdict.sh`,
`fetch-result-context.sh`, `ingest-results-site.sh`, `bench-campaign/README.md`,
`render-campaign-toml.sh`, `run-campaign.sh`, `slack-recap.jq`,
`tests/test_workflow.py`, `tests/test_notify.py`, the four poll jobs, the
notify and cleanup jobs.

Stays: `.github/workflows/ec2-leg.yml`, `load-test-coordinator.yml`,
`PE/gather`, `harness/gather.go`, `harness/poller.go`, `bootstrap-common.sh`
with `upload_result` and `RESULT_KEY` (gather reads the result object through
`PollerConfig.ResultKey`). Spec Section 9 lists `upload_result` and
`RESULT_KEY` as removed. That row is wrong; correct it in this PR.

### 4.8 `bench-scripts.yml` (CI job)

Triggers: `pull_request` and `push` to `feature/full-history`, `paths`:
`PE/bench-campaign/**`, `.github/workflows/bench-*.yml`. One job on
`ubuntu-latest`: `actions/setup-python@v5` (`python-version: '3.11'`);
`python -m unittest discover -s $BC/tests -t $BC/tests -v`; `shellcheck
$BC/*.sh` (preinstalled on the runner image); actionlint (download one pinned
release binary, about 5 s; decide the version in PR) on
`.github/workflows/bench-*.yml`. `jq` and `bash` are preinstalled.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `.github/workflows/bench-campaign.yml` | modify (rewrite) | Nine inputs, one launch job | 120 |
| `BC/plan-campaign.sh` | new | plan, validate, estimate, ceiling | 55 |
| `BC/launch-box.sh` | new (from #19) | Tags, `deadline`, launch | 50 |
| `.github/workflows/bench-reaper.yml` | new (from #18) | Schedule, dispatch, alert | 50 |
| `BC/reap-boxes.sh` | new | Reaper selection and terminate | 40 |
| `BC/slack-reaper.jq`, `BC/slack-payload.sh` | new (from #17), modify | Reaper card | 41 |
| `.github/workflows/bench-scripts.yml` | new | Script CI | 45 |
| `PE/relay/main.go`, `PE/harness/relay.go`, `env.go`, `harness.go`, `poller.go` | delete / modify | Relay removal | -140 |

## 6. Tests

`BC/tests/`, stdlib `unittest`, `support.py` from #15 with the PR 08 helpers.

| Test | File | Proves | How |
|---|---|---|---|
| `test_plan_outputs_ceiling` | `test_plan.py` | `ceiling_minutes = estimate + 60`, id, instance type | Stub `bench-campaign` writes a `campaign.json`; `estimate` prints `90`. |
| `test_plan_failure_writes_no_output` | `test_plan.py` | `::error::` line, exit 1, no `GITHUB_OUTPUT` | Stub `plan` exits 2 with `bench-campaign: runs: ...`. Pattern of #15 `test_invalid_inputs_produce_no_outputs`. |
| `test_bad_estimate_rejected` | `test_plan.py` | Non-integer estimate fails | Stub prints `abc`, `0`, `-1`. |
| `test_workers_passed_only_when_set` | `test_plan.py` | Default workers come from `plan` | Stub records its argv. |
| `test_launch_tags_and_deadline` | `test_launch.py` | Tags, `terminate`, `deadline = now + (ceiling + 30) * 60` | `aws` and `date` stubs; parse the recorded `run-instances` argv. |
| `test_deadline_selection`, `test_no_expired_instances`, `test_aws_errors_fail_the_job` | `test_reaper.py` | #18 cases | Run `reap-boxes.sh` itself (no YAML slicing, spec Section 11 rule 9). |
| `test_reaper_long_and_empty` | `test_slack.py` | Reaper card limits | From #17. |

Go: `go test ./cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/...`
proves the relay removal leaves `gather` and `harness` green.

## 7. Done when

- One campaign with `publish=no` runs on EC2, uploads to S3, posts to Slack
  and powers off. Check: `aws s3 ls s3://stellar-rpc-bench/results/<id>/`
  lists `campaign.json`, `steps/` and `logs/box.log`; the Slack message
  exists; `aws ec2 describe-instances --instance-ids <id>` shows
  `terminated` before the ceiling.
- A manual reaper dispatch terminates a test box with a past deadline. Check:
  launch a `t3.nano` with the three tags and `deadline=1`; run
  `gh workflow run bench-reaper.yml`; the box is `terminated`.
- CI runs the script tests: `bench-scripts.yml` is green on the PR.
- The first run records the size of each hot dataset:
  `jq '.steps[] | select(.kind=="ingest-hot") | {name, datasetBytes}'` on the
  uploaded `campaign.json` (field from PR 07). Copy the values into the spec
  (Section 1.1 or Q7 notes).

## 8. Verification before push

- `python3 -m unittest discover -s $BC/tests -t $BC/tests -v`
- `shellcheck $BC/*.sh`; `actionlint .github/workflows/bench-*.yml`
- `go build ./...`; `go vet ./...`
- `go test -race ./cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/...`
- `make go-check-branch BASE=feature/full-history` (catches an unused
  `unixTime` or helper left after the relay removal).
- `CGO_ENABLED=0 go build ./cmd/bench-campaign` (the GitHub runner path).

## 9. Risks and open points

- Q2 blocks the EC2 "Done when" items, not the merge.
- The dispatch form on `main` shows old inputs until the `main` stub PR
  merges. Dispatch always on `feature/full-history`.
- The reaper has no schedule until `feature/full-history` merges into
  `main`. The box ceiling is the only automatic net until then (D16 (5)).
- The GHA role must allow `ec2:RunInstances` with the new `campaign-id` tag.
  The terminate grant is conditioned on the `test` tag (facts), so it holds.
  Verify tag-on-create permission for the extra tag.
- A campaign longer than the 6-hour job limit is fine: no job waits.
- Decision log in this PR: correct Section 9 (`upload_result`, `RESULT_KEY`
  stay for `ec2-leg.yml`); record the `campaign-id` tag and the move of the
  reaper body into `reap-boxes.sh`.
