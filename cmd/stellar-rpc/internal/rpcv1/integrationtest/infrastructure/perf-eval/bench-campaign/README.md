# Benchmark campaign automation

A campaign is one `stellar-rpc-benchmarks` campaign on one ephemeral EC2 box.
The `Bench campaign (EC2)` workflow launches, waits for, and reports it; the
benchmarks live in `stellar-experimental/stellar-rpc-benchmarks`. The harness
runs at the dispatched commit; the benchmarked `ref` is resolved on the box.

## Lifecycle

1. `validate` checks the dispatch inputs and renders the campaign TOML.
2. `launch` checks that the fresh result key returns NoSuchKey, renders the gzipped
   user-data (preamble + `bootstrap-common.sh` + `run-campaign.sh`), boots the
   box, and waits for its SSM agent.
3. `poll1`..`poll4` relay the wait across four job windows; exactly one window
   sees the result object the box publishes.
4. `notify` decides the verdict, reads this attempt's context from S3, ingests
   a passing run into the results site, and posts the Slack card.
5. `cleanup` terminates the box unless the campaign failed, in which case the
   box stays up to its self-terminate ceiling for rescue over SSM.
6. `bench-reaper.yml` terminates tagged boxes past their `deadline` tag.

Each attempt uses `runs/<run_id>/<run_attempt>/campaign/` in the CI bucket.
An old box cannot overwrite a later attempt's marker or sidecar.
Relay faults and box failures both return `fail`; the polling diagnostics distinguish them.
The final window fails if it returns `running` without a verdict.

## Compatibility and limits

The runner and converter exchange a data bundle. Matching Git commits is not
required for compatibility. `run-info.json` records the execution commit as
`benchmarksSha`; ingest logs its initial converter checkout separately.
The selected benchmarks ref is resolved again during ingest. Publication from
`main` uses the sibling ingest script's retry on concurrent result commits.
Other refs convert locally and report `skipped`, without publication.

The sibling converter does not yet enforce all input schema versions. It also
reads assessment targets from its checkout. Format validation and assessment
policy belong to that repository; this stack does not add a commit-equality gate.
Only dispatch trusted refs: box scripts execute with the instance role, and
ingest executes with AWS credentials and the site push token.

The 1260-minute ceiling covers paced work plus a fixed 120-minute allowance for
setup, cold ingest, and uploads. Cold work has no measured duration bound here.
Query estimates allow six txhash rates and three rates for each other endpoint.
Early bootstrap failure before AWS CLI installation can leave the result absent
until the relay deadline. Shutdown and the tagged reaper are the backstops.
The workflow role needs `s3:GetObject` and effective `s3:ListBucket` permission
for absent result keys to return 404 instead of 403. The pre-launch access check
fails closed on denied access or an existing result; it never writes a pending
marker. Upstream #936 at `927d4c17` accepts only `ok` and `fail` producer verdicts.
Relay waits through absent/stale objects and transient failures, but reports
permanent request errors immediately. IAM changes are not part of this stack.
Live IAM permissions, six-hour OIDC sessions, EC2, S3, and Slack require operational
verification. Offline tests do not prove those integrations.

## Offline verification

From the repository root, with Python 3.11+, Bash, jq, ShellCheck, and actionlint:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/bench-campaign/tests -v
shellcheck cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/bench-campaign/*.sh
shellcheck -s bash -e SC2016 cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/bootstrap-common.sh
actionlint .github/workflows/bench-campaign.yml .github/workflows/bench-reaper.yml
AWS_EC2_METADATA_DISABLED=true go test ./cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/...
```

Shell tests use an isolated command path and environment. AWS, Git, and curl are
stubbed when used. No test launches infrastructure, publishes data, or posts a
notification. ShellCheck and actionlint are local checks, not repository CI jobs.

## File map

| Path | Role |
|---|---|
| `render-campaign-toml.sh` | Validates inputs, computes the budget, emits the TOML and launch parameters. |
| `render-user-data.sh` | Assembles and gzips the user-data; fails on EC2's 16 KB cap. |
| `run-campaign.sh` | Sourced fragment run on the box: build, campaign, bundle upload, result object. |
| `launch-box.sh` | Runs the instance with the tags cleanup, the GHA role and the reaper rely on. |
| `check-result-key.sh` | Requires a fresh key and NoSuchKey response before launch; detects missing read/list permissions. |
| `gate-relay-state.sh` | Turns the relay's `state` into a poll job's pass, handoff or failure. |
| `decide-verdict.sh` | Reduces the four poll states and the validate/launch results to `state`, `reason`, `rescued`. |
| `fetch-result-context.sh` | Reads this attempt's run-info sidecar (ok) or verdict excerpt (fail) from S3. |
| `ingest-results-site.sh` | Converts and publishes a passing bundle; reports `ingest_state` / `ingest_reason`. |
| `slack-payload.sh` | Renders the Block Kit payload for `MODE=campaign` or `MODE=reaper`. |
| `slack-campaign.jq`, `slack-reaper.jq` | The two card layouts. |
| `slack-recap.jq` | The ingestion-vs-target recap read from a run's site JSON. |
| `post-slack.sh` | Renders via `slack-payload.sh` and posts; degrades to a warning, never fails. |
| `../bootstrap-common.sh` | Box bootstrap shared with the other perf-eval workflows. |
| `.github/workflows/bench-campaign.yml` | The campaign workflow. Inline steps: summarize plan, wait for SSM, terminate. |
| `.github/workflows/bench-reaper.yml` | Scheduled sweep of overdue boxes; untagged boxes are reported, not killed. |
| `.github/actions/bench-poll/action.yml` | One relay window: Go toolchain, AWS credentials, the relay, the step summary. |
| `perf-eval/relay` | The relay command. |
| `perf-eval/harness` | The relay/gather poller and its tests. |

Scripts take their inputs from the environment and append outputs to
`GITHUB_OUTPUT`, or to stdout when it is unset, so every step runs on a laptop.

## Run it locally

From this directory:
```
PHASE=1 INGEST=both QUERY=no MACHINE=2x ./render-campaign-toml.sh
S1=ok S2= S3= S4= VALIDATE_RESULT=success LAUNCH_RESULT=success ./decide-verdict.sh
MODE=campaign STATE=ok CAMPAIGN_NAME=phase1-2x ./slack-payload.sh | jq .
```

`fetch-result-context.sh` and `ingest-results-site.sh` need AWS credentials that
can read `$BUCKET`, the CI bucket the box uploads the result object and tarball
to; the ingest also needs `PUSH_TOKEN`. Relay tests: `go test
./cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/harness/...`
from the repository root.
