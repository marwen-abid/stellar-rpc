# Benchmark campaign automation

A campaign is one `stellar-rpc-benchmarks` campaign running on one ephemeral
EC2 box. The GitHub Actions workflow only launches, drives, observes, and
reports that campaign; the benchmark implementation lives in the benchmarks
repository.

## Lifecycle

1. Validate dispatch inputs and render the campaign TOML.
2. Launch the box with user-data made from a preamble, `bootstrap-common.sh`,
   and `run-campaign.sh`.
3. Relay polling through four GitHub Actions job windows.
4. Derive the verdict, publish passing results, and notify Slack.
5. Terminate the box unless a failed run is being left briefly for rescue.
6. Let the scheduled reaper terminate any box that survives its deadline.

## Field map

| Path | Role |
|---|---|
| `render-campaign-toml.sh` | Validates inputs and emits TOML plus launch parameters. |
| `render-user-data.sh` | Combines the preamble and box-side scripts within EC2's size limit. |
| `run-campaign.sh` | Runs the campaign on the box and publishes its result objects. |
| `launch-box.sh` | Launches and tags the campaign EC2 instance. |
| `gate-relay-state.sh` | Converts a relay state into pass, handoff, or failure. |
| `decide-verdict.sh` | Reconstructs the final verdict from the poll chain. |
| `fetch-result-context.sh` | Reads the matching run sidecar or failure excerpt from S3. |
| `ingest-results-site.sh` | Pushes a passing bundle through the results-site ingest. |
| `../bootstrap-common.sh` | Shared box bootstrap that user-data prepends to `run-campaign.sh`. |
| `slack-payload.sh` | Selects and renders a Slack Block Kit payload. |
| `slack-campaign.jq` | Defines the campaign notification layout. |
| `slack-reaper.jq` | Defines the reaper notification layout. |
| `slack-recap.jq` | Extracts the ingestion-vs-target recap from a run's site JSON. |
| `post-slack.sh` | Posts a rendered payload without changing the verdict. |
| `.github/workflows/bench-campaign.yml` | Orchestrates one campaign. |
| `.github/workflows/bench-reaper.yml` | Reaps overdue campaign boxes. |
| `.github/actions/bench-poll/action.yml` | Runs one relay polling window. |
| `perf-eval/relay` | Command wrapper for the harness relay poller. |
| `perf-eval/harness` | Relay and gather implementation, with the relay unit tests. |

## Run it locally

From this directory, try `PHASE=1 INGEST=both QUERY=no MACHINE=2x
./render-campaign-toml.sh` or `MODE=campaign STATE=ok CAMPAIGN_NAME=test
./slack-payload.sh | jq .`. Run relay tests from the repository root with
`go test ./cmd/stellar-rpc/internal/rpcv1/integrationtest/infrastructure/perf-eval/harness/...`.
