# PR 08: Box bootstrap, user-data stub and box script

| | |
|---|---|
| Branch | `bench-campaign-v2/08-box` |
| Repository | marwen-abid/stellar-rpc (base `feature/full-history` at `91f158b` or later) |
| Depends on | PR 07 (`bench-campaign run`, `campaign.json` updates), Q2 (secret store, IAM grant). B2 must land before the first run with `publish=yes`. |
| Implements | D16, D17, D31; spec Sections 4, 6.6, 7.1 |
| Estimate | about 540 non-test lines: `box-bootstrap.sh` 105, `user-data-stub.sh` 45, `render-user-data.sh` 40, `run-box.sh` 175, `slack-payload.sh` 75, `slack-campaign.jq` 70, `post-slack.sh` 29 |

`PE` = `cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval`; `BC` = `PE/bench-campaign`.

## 1. Goal

After this PR, an EC2 box runs a campaign from start to end, and no GitHub
job waits for it. The user-data stub sets the ceiling, gets `campaign.json`
from S3, clones stellar-rpc and runs the bootstrap and the box script. The box
script runs `bench-campaign run`, uploads each finished step to S3, pushes
(`publish=yes`), posts to Slack and powers off.

## 2. Scope

- Port benchmarks `runner/bootstrap.sh` to `BC/box-bootstrap.sh`.
- Add the fetch-and-run stub `BC/user-data-stub.sh` and rewrite
  `BC/render-user-data.sh` (from #16) to emit preamble + stub, gzipped.
- Add the box script `BC/run-box.sh`: runner launch, per-step S3 upload,
  crash marking, finish order, secrets from AWS.
- Rewrite `BC/slack-payload.sh` and `BC/slack-campaign.jq` to read
  `campaign.json`. Take `BC/post-slack.sh` from #17.
- Add Python tests in `BC/tests/` that run the scripts with stub commands.

## 3. Out of scope (boundaries)

- The workflow, `launch-box.sh`, `bench-campaign plan`/`estimate` calls, the
  upload of `campaign.json` before launch, the reaper and the CI job that runs
  these tests: PR 09.
- `slack-reaper.jq` and the reaper mode of `slack-payload.sh`: PR 09.
- The `bundle/**` workflow in the benchmarks repository: B2.
- Changes to `PE/bootstrap-common.sh` (see Section 9). The stub copies three
  of its pieces; it does not source the file.
- Load-step tools (Blaster build, cache drop): PR 10, inside the runner.
- `render-campaign-toml.sh`, `run-campaign.sh`, `slack-recap.jq`,
  `run-info.json`, the `published:` grep: not ported (spec Section 9).

## 4. Design

### 4.1 `render-user-data.sh` (from #16, rewritten)

Env in: `CAMPAIGN_ID`, `RUNNER_REPO` (`owner/repo`), `RUNNER_COMMIT` (40
hex), `CEILING_MINUTES`, `OUT` (default `/tmp/user-data.sh`). Out: `$OUT` and
`$OUT.gz`. It writes `#!/usr/bin/env bash`, one `printf 'export K=%q\n'` line
for each of the four values, then `cat user-data-stub.sh`. There is no base64
data (D16 item 4): PR 09 uploads `campaign.json` to
`s3://stellar-rpc-bench/results/<id>/campaign.json` before the launch (D31).
It keeps `gzip -9` and the check `[ "$SIZE" -le 16384 ]` with the message
`16384-byte EC2 limit`. It rejects a `RUNNER_COMMIT` that is not 40 hex
characters and a `CEILING_MINUTES` that is not a positive integer.

### 4.2 `user-data-stub.sh`

1. `shutdown -P "+${CEILING_MINUTES}" "bench-campaign ceiling"` is the first
   command after the preamble (from `bootstrap-common.sh` lines 20 to 21).
2. `exec > >(tee -a /var/log/user-data.log | logger -t user-data -s
   2>/dev/console) 2>&1` (from `bootstrap-common.sh` line 15).
3. `BOOT_EPOCH=$(date +%s)`; `HOME=/root USER=root`; apt `jq git curl
   ca-certificates unzip`; the AWS CLI (apt, else the v2 bundle, from #16).
4. `aws s3 cp s3://stellar-rpc-bench/results/$CAMPAIGN_ID/campaign.json
   /root/campaign.json`. Then `git fetch --depth 1
   https://github.com/$RUNNER_REPO $RUNNER_COMMIT` into `/root/stellar-rpc`.
5. `bash $BC/box-bootstrap.sh /root/campaign.json`, then `exec bash
   $BC/run-box.sh /root/campaign.json`.

ERR trap before step 5: log the line, copy `/var/log/user-data.log` to
`s3://stellar-rpc-bench/results/$CAMPAIGN_ID/logs/box.log` if `aws` exists,
then `poweroff`. There is no data to keep yet, so the box does not wait for
its ceiling.

### 4.3 `box-bootstrap.sh` (port of benchmarks `runner/bootstrap.sh`, 174 lines)

Keep, in this order: instance-store discovery by `lsblk -dno NAME,MODEL`
model `Instance Storage` (one disk, else exit 1; `NVME_DEV` override); the
model check; `mkfs.ext4 -m0` when `blkid` finds no filesystem; `mount -o
noatime` at `MOUNT` (default `/mnt/nvme`); `mkdir -p $MOUNT/bench`; the fsync
probe (`dd ... oflag=dsync`, fail on `GB/s`, `FSYNC_PROBE_WARN_ONLY=1`);
apt packages `build-essential curl git jq pkg-config cmake ninja-build unzip
libsnappy-dev liblz4-dev zlib1g-dev`; the AWS CLI check; Go `go1.26.5` at
`/usr/local/go`; Rust `1.92.0` by rustup; zstd and RocksDB.

Drop: `sudo`, `tmux`, gcloud, the `golden,scratch,hot,results` directories,
the `$BENCH_ROOT/src` clone and the `.bashrc` block. Write `/root/box-env.sh`
(`PATH`, `CGO_CFLAGS`, `CGO_LDFLAGS`, `LD_LIBRARY_PATH`, `RUSTUP_TOOLCHAIN`)
for `run-box.sh` to source.

Native libs: `scripts/install-zstd.sh` (zstd 1.5.7) and
`scripts/install-rocksdb.sh` (RocksDB 10.9.1, for grocksdb v1.10.7) exist in
stellar-rpc at `91f158b` (verified). The pins must match the grocksdb of
`ref`, not of the workflow commit. Choice: fetch `ref` (`.inputs.ref` of
`campaign.json`) into the clone and run `git show
FETCH_HEAD:scripts/install-rocksdb.sh` and `...install-zstd.sh` with
`PREFIX=$HOME/.rocksdb ZSTD_HOME=$HOME/.zstd`. If `ref` has no such script,
use the workflow commit's copy and log it.

`RUSTUP_TOOLCHAIN=1.92.0` is necessary: stellar-rpc has
`rust-toolchain.toml` with `channel = "stable"`, which overrides the rustup
default inside the tree.

### 4.4 `run-box.sh <campaign.json>`

Constants: `ROOT=${ROOT:-/mnt/nvme/bench}`,
`BUNDLE=$ROOT/results/$CAMPAIGN_ID`,
`PREFIX=s3://stellar-rpc-bench/results/$CAMPAIGN_ID`,
`BENCHMARKS_REPO=stellar-experimental/stellar-rpc-benchmarks`,
`WATCH_SECONDS=${WATCH_SECONDS:-60}`.

Functions (one cleanup path, set in `main`):

| Function | Job |
|---|---|
| `build_runner` | `go build -o /usr/local/bin/bench-campaign ./cmd/bench-campaign` in `/root/stellar-rpc`. |
| `watch_steps` | Loop: read `$BUNDLE/campaign.json` with `jq`. For each step with `status` in `ok`, `failed`, `skipped` and not in `$BUNDLE/.uploaded`, run `aws s3 cp --recursive $BUNDLE/<path> $PREFIX/<path>`, then `aws s3 cp campaign.json`. Append the name to `.uploaded`. On a failure, log it and retry on the next pass. |
| `mark_crashed <rc>` | `jq '(.steps[] \| select(.status=="running")) \|= (.status="crashed" \| .error="runner exited: <rc>")'` to a temporary file, then `mv`. |
| `sync_bundle` | Copy `/var/log/user-data.log` to `logs/box.log`. `aws s3 sync $BUNDLE $PREFIX --exclude .uploaded`. Sets `S3_STATUS=ok\|failed`. |
| `fetch_secret <id>` | Prints one secret value. Backend: decide with Q2 (4.5). |
| `push_bundle` | Only with `.inputs.publish == "yes"`. Sets `PUSH_STATUS=pushed\|failed\|skipped\|no-token`. |
| `notify` | `WEBHOOK=$(fetch_secret ...)` then `post-slack.sh`. Never fails. |
| `finish` | `sync_bundle`; `push_bundle`; `notify`; if `S3_STATUS=ok`, `poweroff`; else log "left up until the ceiling" and exit 1. |

`main`: `mkdir -p $BUNDLE`; `cp campaign.json $BUNDLE/`; source
`box-env.sh`; `build_runner`; start `watch_steps &`; run
`timeout --kill-after=5m <left>s bench-campaign run --root $ROOT
$BUNDLE/campaign.json`, where `<left>` = `BOOT_EPOCH + CEILING_MINUTES*60 -
1800 - now`. This leaves 30 minutes for the finish. Keep `rc`. Stop the
watcher, run one last `watch_steps` pass, `mark_crashed` when `rc != 0`, then
`finish`. A failed build (`build_runner`) also goes to `finish` with no step
`running`.

Runner contract (agreed with the PR 07 author): bundle dir =
`dirname(campaign.json)`, rewritten in place; a step is final at `ok`,
`failed` or `skipped`; exit 0 = all ok or skipped, 1 = a step failed, 2 =
setup error, every step `pending`.

Failure rules (D16): only an S3 failure keeps the box up. The ERR trap calls
`finish`.

### 4.5 Secrets (Q2 open)

The script needs exactly two values:

| Name (proposed id) | Value | Used by |
|---|---|---|
| `stellar-rpc-bench/benchmarks-push-token` | Fine-grained token, `contents: write` on `stellar-experimental/stellar-rpc-benchmarks` only. A token push triggers the B2 workflow; a `GITHUB_TOKEN` push would not. | `push_bundle` |
| `stellar-rpc-bench/slack-webhook` | Slack incoming webhook URL (same value as the GitHub secret `SLACK_BENCH_WEBHOOK_URL`). | `notify` |

Backend, decide in PR after Q2: Secrets Manager (`aws secretsmanager
get-secret-value --secret-id <id> --query SecretString --output text`, grant
`secretsmanager:GetSecretValue` on the two ARNs) or Parameter Store (`aws
ssm get-parameter --name /<id> --with-decryption --query Parameter.Value
--output text`, grant `ssm:GetParameter` and `kms:Decrypt`). The role
`stellar-rpc-ci-load-test` also needs `s3:PutObject`/`s3:ListBucket` on
`stellar-rpc-bench/results/*` (the facts say it has them; verify). A missing
secret sets `PUSH_STATUS=no-token` or skips Slack.

### 4.6 `push_bundle`

1. `git clone --depth 1 --branch main` the benchmarks repository with
   `-c http.https://github.com/.extraheader="AUTHORIZATION: basic <base64
   x-access-token:TOKEN>"`. The token is never in a URL or `.git/config`.
2. `git switch -c bundle/$CAMPAIGN_ID` from `main`: a push trigger reads the
   workflow file (B2) from the pushed commit.
3. Copy `$BUNDLE` without `logs/` and `.uploaded` to `bundles/$CAMPAIGN_ID/`
   (`tar --exclude`). This path is the contract with B2.
4. Commit `bundle: <id>`. Push up to 5 times, sleep `5 * attempt` seconds
   between attempts.

### 4.7 Slack (`slack-payload.sh`, `slack-campaign.jq`, `post-slack.sh`)

`post-slack.sh` (#17) stays as is (`curl --max-time 30`, degrades to a warning).
`slack-payload.sh` env in: `CAMPAIGN_JSON`, `S3_PREFIX`, `S3_STATUS`,
`PUSH_STATUS`, `RUNNER_EXIT`, `BOX_ID`, `CEILING_EPOCH`, `RUN_URL`,
`AWS_REGION`. `run-box.sh` builds `RUN_URL` from `RUNNER_REPO` and the GitHub
run id at the end of `id`, because the user-data does not carry it. It keeps `s3_prefix_url` and `fmt_epoch_hm` from #17 and runs
`jq -f slack-campaign.jq --slurpfile c "$CAMPAIGN_JSON"`. The card holds: the
id; `ref` and `commit`; machine and close interval; one line per step with
status and `error` (cut to 200 characters); the S3 prefix link; the log keys
`logs/box.log` and `logs/runner.log`; the push result, with the S3 path on a
failure; the box state (powered off, or up until the ceiling time). State
`ok` needs runner exit 0, every step `ok` or `skipped`, and `S3_STATUS=ok`.
Keep the block limits from #17 (header 150, section 3000, field 2000).

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `BC/box-bootstrap.sh` | new | Port of benchmarks `runner/bootstrap.sh` | 105 |
| `BC/user-data-stub.sh` | new | Ceiling, log, clone, exec | 45 |
| `BC/render-user-data.sh` | new (from #16) | Preamble + stub, gzip, 16 KB check | 40 |
| `BC/run-box.sh` | new | Runner, uploads, crash, finish | 175 |
| `BC/slack-payload.sh` | new (rewrite of #17) | Env to jq args | 75 |
| `BC/slack-campaign.jq` | new (rewrite of #17) | Card from `campaign.json` | 70 |
| `BC/post-slack.sh` | new (from #17) | Post, degrade to warning | 29 |
| `BC/tests/*.py` | new | Script tests (not counted) | 0 |

## 6. Tests

All tests are stdlib `unittest`, Python 3.11+, in `BC/tests/`. `support.py`
comes from #15 (51 lines). Add to it: tools `sleep`, `cp`, `mv`, `tar`,
`env`, `timeout`, `kill`; a `stub_logged(name, body)` helper that appends
`name args` to `$HOME/calls`; `calls()` returns that list. No test slices a
script's text (spec Section 11 rule 9).

| Test | File | Proves | How |
|---|---|---|---|
| `test_user_data_quotes_and_compresses` | `test_user_data.py` | Preamble quoting, gzip round trip, size under cap, only the four values and no base64 data | From #16 `test_box.py`; run the whole rendered file with a `shutdown` stub that prints `env` and exits. |
| `test_user_data_rejects_oversize`, `test_user_data_rejects_bad_commit` | `test_user_data.py` | 16 KB cap; input check | From #16 (20,000 random bytes); `RUNNER_COMMIT=main`. |
| `test_stub_sets_ceiling_first` | `test_user_data.py` | Ceiling before any other command | Run the rendered file with stubs for `shutdown`, `apt-get`, `git`, `aws`, `logger`, `bash` target; `calls()[0]` is `shutdown -P +N`. |
| `test_stub_gets_campaign_from_s3` | `test_user_data.py` | `aws s3 cp` of `results/<id>/campaign.json` comes before the `git` fetch | Same stubs; check the order in `calls()`. |
| `test_stub_clone_failure_powers_off` | `test_user_data.py` | Early failure: log upload, `poweroff` | `git` stub exits 1; also `aws s3 cp` of `campaign.json` exits 1. |
| `test_bootstrap_refuses_non_instance_store`, `test_bootstrap_fsync_probe`, `test_bootstrap_skips_mkfs_and_mount` | `test_bootstrap.py` | Model check; `GB/s` fails unless `FSYNC_PROBE_WARN_ONLY=1`; idempotent re-run | Stubs `lsblk`, `dd`, `blkid`, `mountpoint`, `mkfs.ext4`, `mount`; `MOUNT` = temp dir. |
| `test_finish_order_success` | `test_box.py` | S3, push, Slack, box log, `poweroff`, in order | Fake `bench-campaign` writes two `ok` steps; assert the order in `calls()`. |
| `test_step_uploaded_before_exit` | `test_box.py` | Per-step upload | Fake runner writes step 1 `ok`, sleeps 3 s; `WATCH_SECONDS=1`; the `aws s3 cp` of step 1 comes before the runner exits. |
| `test_runner_crash_marks_crashed` | `test_box.py` | `running` becomes `crashed`; box still finishes | Fake runner leaves a step `running`, exits 137. Check the uploaded `campaign.json` and the Slack payload. |
| `test_s3_failure_keeps_box_up` | `test_box.py` | No `poweroff`; Slack says so | `aws s3 sync` stub exits 1. |
| `test_push_retries_then_powers_off` | `test_box.py` | 5 push attempts; Slack gives the S3 path; `poweroff` | `git push` stub exits 1. |
| `test_slack_failure_still_powers_off` | `test_box.py` | Slack failure is not fatal | `curl` stub exits 22. |
| `test_publish_no_skips_push`, `test_missing_secret` | `test_box.py` | D17; `no-token` path still powers off | `publish: "no"`; secret stub exits 1. |
| `test_setup_error_exit_2`, `test_push_excludes_logs` | `test_box.py` | Exit 2 marks no step; the pushed tree has no `logs/` | Fake runner exits 2; `git add` stub lists the tree. |
| `test_card_from_campaign_json`, `test_block_limits` | `test_slack.py` | Id, statuses, failed steps, S3 link, log keys, push result; Slack limits | Sample `campaign.json`; #17 `payload()` helper. |
| `test_post_failure_preserves_summary`, `test_missing_webhook_never_calls_curl` | `test_slack.py` | `post-slack.sh` degrades | From #17. |

## 7. Done when

- The script tests pass: `python3 -m unittest discover -s BC/tests -t BC/tests`.
- They cover a runner crash (`test_runner_crash_marks_crashed`), an S3
  failure (`test_s3_failure_keeps_box_up`), a push failure
  (`test_push_retries_then_powers_off`) and a Slack failure
  (`test_slack_failure_still_powers_off`).
- They check the finish order of Section 4 (`test_finish_order_success`).

## 8. Verification before push

- `python3 -m unittest discover -s $BC/tests -t $BC/tests -v`
- `shellcheck $BC/*.sh`; `bash -n $BC/*.sh`
- `bash $BC/render-user-data.sh` on a real PR 06 `plan` output; record the gzipped size.
- `go build ./...`; `make go-check-branch BASE=feature/full-history` (no Go change expected).
- `git diff --numstat feature/full-history -- . ':!*_test.go' ':!*/tests/*' ':!*.md'`; sum the first column (D32).

## 9. Risks and open points

- Q2 blocks the first EC2 run: it needs the store and the IAM grant.
- Size: 540 lines. Over 600, split into 08a `box-bootstrap` (bootstrap, stub,
  render; about 190) and 08b `box-finish` (`run-box.sh`, Slack; about 350).
- `PE/bootstrap-common.sh` keeps `upload_result` and `RESULT_KEY`, because
  `ec2-leg.yml` and `load-test-coordinator.yml` use them (spec Section 9).
- `bench-serve.log` in a load step directory is pushed. Check its size;
  GitHub rejects files over 100 MB.
- RocksDB builds from source on each box (not measured); `setup_minutes`
  must cover it. `RUNNER_COMMIT` must be fetchable anonymously; verify.
- The `bundles/<id>/` path, the branch from `main`, the 30-minute finish
  margin, the native libraries from `ref` and `RUSTUP_TOOLCHAIN` are in spec
  6.6 and D16. Change them only with the spec and B2.
