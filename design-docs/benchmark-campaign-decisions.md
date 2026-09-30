# Benchmark Campaigns: Decision Log

This log records the decisions behind [benchmark-campaign.md](./benchmark-campaign.md),
the reasons for them, and the facts they depend on. Update it when a decision
changes. Decisions were made by Marwen Abid on 2026-09-29 and 2026-09-30.

Status values:

- **Confirmed**: Marwen made or approved the decision.
- **Proposed**: the spec contains it, but Marwen did not confirm it
  explicitly. Confirm it before you build on it.

## 1. Decisions

| ID | Decision | Status | Reason | Rejected options |
|---|---|---|---|---|
| D1 | Drop `bench-query` (PRs #12 to #14). | Confirmed | It measures storage only, one endpoint at a time. It opens the hot database read-write, so it changes the dataset. It leaves dropped requests out of its percentiles. About half of it is a load generator, which Blaster already is. | Keep and fix it. |
| D2 | Use Blaster for endpoint load. | Confirmed | Blaster sends mixed traffic with production-derived shapes, ramps the rate, and reports per step. | Build our own load generator. |
| D3 | Add `bench-serve`: the daemon's JSON-RPC handlers over a dataset, with no ingestion. | Confirmed | Blaster needs an HTTP server. The daemon has no serve-only mode: startup needs captive core and runs `backfillToTip` before it serves. | Change daemon startup. |
| D4 | `bench-ingest` keeps its catalog in the dataset. | Proposed | Without the catalog, nothing can open the dataset. The old cold fixture rebuilt the catalog from file names, which drifts from the daemon. | Rebuild the catalog in `bench-serve`. |
| D5 | Add a read-only hot open that serves events. | Proposed | A read-write open replays the WAL and can compact, so it changes the dataset. `hotchunk.OpenReadOnly` has no events facade. | Copy the dataset before each run. |
| D6 | Storage metrics go in the shared read path, and production exposes them. | Confirmed | They show the storage share of each request under load. The label set is small. The code path is the same for production and `bench-serve`. | `bench-serve` only. |
| D7 | No `bench-read` (isolated in-process storage benchmark). | Confirmed | No target depends on it. The storage metrics give the storage share. | Build it now or later. |
| D8 | Campaign orchestration (workflow, EC2, box) stays in stellar-rpc. | Confirmed | stellar-rpc-benchmarks is in the stellar-experimental organization and has no AWS access. | Move orchestration to the benchmarks repository. |
| D9 | stellar-rpc measures; stellar-rpc-benchmarks judges. The results bundle is the only contract. | Confirmed | Phases and targets are SDF-specific. They stay public, but separate from the main codebases in the stellar organization. | Move the judging into stellar-rpc. |
| D10 | The campaign input is the ledger close interval, not the phase. stellar-rpc hard-codes the dataset profiles for each interval for now. The benchmarks repository maps intervals to phases. | Confirmed | A phase is only a set of datasets at a close interval. stellar-rpc does not need to know phases. | A `phase` input (#15). |
| D11 | stellar-rpc pushes each finished bundle to the benchmarks repository. | Confirmed | Marwen's choice. | The benchmarks repository pulls bundles; stellar-rpc sends a notification event. |
| D12 | Every bench command writes a documented `results.json` with named fields and units. This includes `bench-ingest`. Remove its CSV files after the converter reads `results.json`. | Confirmed | The converter parses CSV row names with regular expressions, and rates were stored in nanosecond columns. | JSON for new commands only; keep the CSV files. |
| D13 | No query verdicts until the Blaster load step works. | Confirmed | Nothing temporary to maintain. | Keep running the `bench-query` branch in the meantime. |
| D14 | Separate hot and cold datasets. One load step per dataset. No request spans both tiers. | Confirmed | Each run gives one tier's verdict directly. | One mixed dataset; both. |
| D15 | Move the campaign runner from stellar-rpc-benchmarks (`runner/cmd/campaign`) into stellar-rpc (`cmd/bench-campaign`). | Proposed | Running campaigns is stellar-rpc's job under D9. The runner builds and runs the bench commands. | Keep the runner in the benchmarks repository and clone it on the box. |
| D16 | The box finishes its own campaign: it uploads and pushes the bundle, posts to Slack, and terminates itself. The workflow only starts the box. The reaper stays as a safety net. | Proposed | It removes four chained poll jobs (about 21 hours of waiting) and three places that decide pass or fail. | Poll S3 from GitHub Actions (#19). |
| D17 | A `publish` input, default `no`. Only runs with `publish=yes` reach the public site. | Proposed | Today any passing run publishes, including a short test run. | Publish every run. |
| D18 | The Blaster questions stay open. | Confirmed | Marwen deferred them. See the spec, Section 12. | — |
| D19 | Vocabulary (spec, Section 3). Do not use: corpus, leg, cell, shed, pacing, fixture. Do not use "phase" in stellar-rpc. | Confirmed | Marwen found the old words unclear. | — |
| D20 | `bench-serve` reads the network passphrase from a flag. There is no `dataset.json` file. | Proposed | `bench-ingest` does not know the passphrase. The catalog already holds the chunk range and tier. | A `dataset.json` manifest. |

## 2. Facts the decisions depend on

All paths are in stellar-rpc at `feature/full-history` `36ac502`, unless a
line names another repository.

| Fact | Where |
|---|---|
| `bench-ingest` makes a temporary catalog and deletes it when it exits. | `cmd/stellar-rpc/internal/rpcv2/bench/scratch.go`, `openScratchCatalog` |
| The daemon expects the catalog at `<root>/catalog/rocksdb`. | `rpcv2/geometry/paths.go`, `Layout.CatalogPath` |
| Only the index build reads the catalog secret. Readers get the index secret from the file. A new catalog can read an existing dataset. | `rpcv2/backfill/process.go`; `rpcv2/stores/event/cold_format.go` |
| `hotchunk.OpenReadOnly` makes no events facade, because the events warmup runs only on a read-write open. The comment assigns a read-only variant with events to #772. | `rpcv2/stores/hotchunk/hotchunk.go` |
| Daemon startup needs captive core and a backfill backend, and runs `backfillToTip` before it serves reads. | `rpcv2/daemon.go`, `rpcv2/startup.go` (`run`) |
| RPCv2 uses the shared `methods` handlers. `wrapAdapterRequest` gets one read view per request. | `rpcv2/jsonrpc.go` |
| The tier is chosen per chunk in `ReadView.resolveTier`. Cold readers open per read view and close at release. | `rpcv2/query/resolve.go`, `rpcv2/query/tx_lookup.go` |
| The only request timing metric is `json_rpc_request_duration_seconds`, a Summary with labels `endpoint` and `status`. No metric times storage. | `cmd/stellar-rpc/internal/jsonrpc/jsonrpc.go` |
| Candidate timer points for storage metrics: `ReadView.WithLedger`, the `ScanLedgers` iterator, the reader from `ReadView.Events`, `windowGatedIndex.Get`, `lazyColdTxIndex.Get`, the `OpenColdReader` calls. | `rpcv2/query/` |
| A chunk is 10,000 ledgers. Chunk `c` starts at ledger `c × 10000 + 2`. | `rpcv2/chunk/chunk.go` |
| The #15 to #20 stack is based on the unsquashed commits of upstream #936. #936 later merged as one squashed commit, so the stack must be rebuilt on current `feature/full-history`. | branch `bench-campaign/00-relay-poller` |
| The close interval to dataset profile map: `2s`: sac-6000, custom_token-4000, soroswap-1500. `1s`: sac-5000, custom_token-4000, soroswap-1500. `600ms`: sac-6000, custom_token-3600, soroswap-1800. | `bench-campaign/render-campaign-toml.sh` on branch `bench-campaign/06-notify` |
| Blaster's `generate` calls only `getLatestLedger`, `getTransactions` and `getEvents`. | stellar-rpc-blaster `dev`, `internal/generate/seed/` |
| A Blaster config `limit` overrides the page limit on every request. | stellar-rpc-blaster `dev`, `internal/run/parameters/endpoints.go` |
| Targets, phases, traffic mix and load levels are defined in `docs/targets.json`. The converter and the site read them. | stellar-rpc-benchmarks `main`, `docs/targets.json`, `converter/convert.py` |

## 3. Superseded work

These PRs in `marwen-abid/stellar-rpc` are replaced by this design. Do not
merge them. Read them only for the parts listed under "reuse".

| PRs | Content | Reuse |
|---|---|---|
| #12 | `bench-query` scheduler, report schema, shared command setup | The crash-safe `invocation.json` write (`bench/invocation.go`); `newSchemaCSVSink` only until D12 lands. |
| #13 | `bench-query` read fixtures and commands | Page-cache eviction (`evict_linux.go`); event term extraction (`eventTerms` in `query_corpus.go`). |
| #14 | `bench-query` registration and tests | Ledger-pack test helpers in `rpcv2test`. |
| #15 | Campaign config rendering | Input checks. |
| #16 | Box user-data and campaign script | Gzipped user-data, awscli fallback. |
| #17 | Slack messages | The message layouts and the posting script. |
| #18 | Reaper | The reaper workflow. |
| #19 | Campaign workflow | Launch script; the Python tests for the scripts. |
| #20 | Notify and publish | Nothing specific; D16 replaces its flow. |
