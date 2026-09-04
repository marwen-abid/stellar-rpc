# Benchmark campaign automation

A campaign is one `stellar-rpc-benchmarks` campaign on one ephemeral EC2 box.
The `Bench campaign (EC2)` workflow launches, waits for, and reports it; the
benchmarks live in `stellar-experimental/stellar-rpc-benchmarks`. The harness
runs at the dispatched commit; the benchmarked `ref` is resolved on the box.

## Lifecycle

1. `validate` checks the dispatch inputs and renders the campaign TOML.
2. `launch` seeds the pending result marker in S3, renders the gzipped
   user-data (preamble + `bootstrap-common.sh` + `run-campaign.sh`), boots the
   box, and waits for its SSM agent.
3. `poll1`..`poll4` relay the wait across four job windows; exactly one window
   sees the result object the box publishes.
4. `notify` decides the verdict, reads this attempt's context from S3, ingests
   a passing run into the results site, and posts the Slack card.
5. `cleanup` terminates the box unless the campaign failed, in which case the
   box stays up to its self-terminate ceiling for rescue over SSM.
6. `bench-reaper.yml` terminates tagged boxes past their `deadline` tag.

## File map

| Path | Role |
|---|---|
| `render-campaign-toml.sh` | Validates inputs, computes the budget, emits the TOML and launch parameters. |
| `render-user-data.sh` | Assembles and gzips the user-data; fails on EC2's 16 KB cap. |
| `run-campaign.sh` | Sourced fragment run on the box: build, campaign, bundle upload, result object. |
| `launch-box.sh` | Runs the instance with the tags cleanup, the GHA role and the reaper rely on. |
| `gate-relay-state.sh` | Turns the relay's `state` into a poll job's pass, handoff or failure. |
| `decide-verdict.sh` | Reduces the four poll states and the validate/launch results to `state`, `reason`, `rescued`. |
| `fetch-result-context.sh` | Reads this attempt's run-info sidecar (ok) or verdict excerpt (fail) from S3. |
| `ingest-results-site.sh` | Converts and publishes a passing bundle; reports `ingest_state` / `ingest_reason`. |
| `slack-payload.sh` | Renders the Block Kit payload for `MODE=campaign` or `MODE=reaper`. |
| `slack-campaign.jq`, `slack-reaper.jq` | The two card layouts. |
| `slack-recap.jq` | The ingestion-vs-target recap read from a run's site JSON. |
| `post-slack.sh` | Renders via `slack-payload.sh` and posts; degrades to a warning, never fails. |
| `../bootstrap-common.sh` | Box bootstrap shared with the other perf-eval workflows. |
| `.github/workflows/bench-campaign.yml` | The campaign workflow. Inline steps: seed marker, summarize plan, wait for SSM, terminate. |
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
