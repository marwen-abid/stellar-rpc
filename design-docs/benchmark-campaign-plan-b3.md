# B3: Query verdicts from Blaster runs

| | |
|---|---|
| Branch | `bench-campaign-v2/b3-query-verdicts` |
| Repository | stellar-experimental/stellar-rpc-benchmarks (base `main` after B1 and B2) |
| Depends on | B1; PR 10 (`load.json`, `blaster.json`, metrics dumps in the bundle); PR 04 (storage metric names); Q4 (verdict level), Q5 (p99 over all samples); Q1 (pinned Blaster commit) |
| Implements | D2, D9, D13, D14, D22; spec Sections 7.1, 7.4, 8 items 2 to 4 and 6, 10.2 B3 |
| Estimate | about 520 non-test lines: `converter/load.py` 230, `converter/convert.py` 25, `docs/targets.json` 12, `docs/app.js` 170, `docs/summary.js` 80. `tests/smoke/gen-fixtures.py` (+40) is under `tests/`, so D32 does not count it. |

## 1. Goal

After this PR, the converter reads each `load` step of a bundle
(`load.json`, `<level>rps/blaster.json`, `metrics-before.txt`,
`metrics-after.txt`) and writes a new `load` section into the run JSON. Each
method of the traffic mix gets a verdict per tier: it fails when the run
aborted, when its error rate is above a new threshold in `docs/targets.json`,
or when its p99 is above its target. The site shows these verdicts and the
tx-hash index size next to the cold `getTransaction` verdict. D13 ends here.

## 2. Scope

- Add `converter/load.py`: read and check the Blaster files, parse the two
  metrics dumps, build the `load` section and its verdicts.
- Call it from the B1 bundle path in `converter/convert.py`; add `load` to
  `SECTION_ORDER`.
- Add `query_load.blended` to `docs/targets.json`.
- Render the `load` section in `docs/app.js` and the verdict rows in
  `docs/summary.js`.
- Update `SCHEMA.md` and `docs/sla-derivation.md`.

## 3. Out of scope (boundaries)

- The old `queries` section, `verdict_sla`, `verdict_e2e`, `verdict_1x` and
  their renderers: kept for committed runs, unchanged.
- The E2E probe family (in-RPC p99 of `getTransaction`): not judged from a
  Blaster run. The request metric is a Summary; its quantiles cover the whole
  process life, not one run (spec Section 6.4). B3 reports the mean in-RPC
  time (Δ`_sum` ÷ Δ`_count`) as context only.
- Blaster flags, levels, mix, run duration: stellar-rpc PR 10 and
  `load.toml` (D28).

## 4. Design

### 4.1 Inputs per load step (spec Sections 7.1, 7.4)

- `load.json`: `schemaVersion`, `step`, `blasterCommit`, `serveReadySeconds`,
  `benchServeExitCode`, `gomaxprocs`, `status`, `error`, `runs[]` with
  `loadLevelRps`, `rngSeed`, `mix`, `blasterArgs`, `pageCache`, `serveFresh`,
  `cpuSeconds`, `status`, `error`.
- `<level>rps/blaster.json` at Blaster `aadc1a1` (verified in
  `internal/run/metrics/results.go`): top keys `start`, `end`, `seed`,
  `duration_seconds`, `aborted`, `endpoints`. Per endpoint: `total_requests`,
  `success`, `errors`, `target_rps`, `percentiles_ms` (keys `p50.0`, `p95.0`,
  `p99.0`, `p99.9`), `timeline[]` (`target_rps`, `success`, `errors`,
  `error_rate_pct`, `p50_ms`, `p95_ms`, `p99_ms`, `p99.9_ms`); optional
  `limit`, `traffic_profile`, `error_types` (`error_msg`, `error_code`,
  `count`, `time_first_seen`, `time_last_seen`), `archetypes` (getEvents).
- `<level>rps/blaster.toml`: `rng_seed`.
- `metrics-before.txt`, `metrics-after.txt`: Prometheus text.

### 4.2 `load.py`

```python
BLASTER_TOP = {"start", "end", "seed", "duration_seconds", "aborted", "endpoints"}
BLASTER_ENDPOINT = {"total_requests", "success", "errors", "target_rps", "percentiles_ms", "timeline"}
BLASTER_OPTIONAL = {"limit", "traffic_profile", "error_types", "archetypes"}
SUPPORTED_BLASTER = {"<full aadc1a1 hash>"}   # decide with Q1

def read_blaster(path, commit) -> dict        # BlasterSchemaError on a key set mismatch
def read_metrics(path) -> dict[tuple, float]  # (name, sorted labels) -> value
def storage_share(before, after) -> dict      # {method: {"ratio", "by_store_tier"}}
def build_load(bundle, steps, targets) -> dict
```

Contract check (spec Section 7.4, review C7): a top-level or per-endpoint key
outside the two sets fails with `BlasterSchemaError(<commit>, <keys>)`. A
`blasterCommit` outside `SUPPORTED_BLASTER` gives a warning. `seed` in
`blaster.json` must equal `rng_seed` in `blaster.toml` (`tomllib`, Python
3.11); else fail.

Error rate: `errors / total_requests`, per method. `error_types` keys hold
the JSON-RPC error text (`error_msg` is empty for JSON-RPC errors); keep the
map as `{text: count}`. A `getTransaction` `NOT_FOUND` counts as success in
Blaster (12% of its hashes never land); no special case.

Storage share (spec Section 6.4): per method M, Δ `_sum` of
`soroban_rpc_fullhistory_read_store_seconds{method=M}` summed over `store`
and `tier`, divided by Δ `_sum` of
`soroban_rpc_json_rpc_request_duration_seconds{endpoint=M}` summed over
`status`. Keep the per `store`,`tier` ratios. A ratio above 1 is a
timeout signal (the handler keeps running); keep it and warn. The exact
metric names come from PR 04; check them there.

### 4.3 Run JSON `load` section

```jsonc
"load": { "cold"|"hot": { "<unit>": {
  "r<level>": {                     // one per load level, ascending
    "page_cache": "dropped", "serve_fresh": true, "aborted": V(bool as 0/1),
    "methods": { "<method>": {
      "target_rps": 300, "requests": V, "error_rate": V, "error_types": {"<text>": int},
      "p50_ms": V, "p95_ms": V, "p99_ms": V, "p999_ms": V,
      "storage_share": V, "in_rpc_mean_ms": V,
      "timeline": [ ...windows of rep 1... ],
      "archetypes": { "<name>": { "p99_ms": V, "error_rate": V } }   // getEvents only
    }},
    "cpu_s": {"bench_serve": V, "blaster": V}, "gomaxprocs": {...}
  },
  "verdicts": { "<method>": { "level": "r500", "p99_ms": 18.2, "threshold_ms": 20,
                              "error_rate": 0.002, "max_error_rate": 0.01,
                              "aborted": false, "pass": true, "reason": "" } },
  "txhash_index_chunks": 2, "dataset_bytes": 123   // cold only, from the ingest step
}}}
```

`V` is the existing median-low aggregate over runs (`stat()`), so
`validate_run` checks it. Unit and tier come from `campaign.json` `steps[]`
(`profile`, `startChunk`, `tier`, `loadLevelRps`), as in B1. Only `load`
steps with `status: ok` convert; others warn. A run inside `load.json` with
`status: failed` is left out with a warning.

### 4.4 `docs/targets.json`

Add under `query_load` (schema stays 2; the key is additive):

```json
"blended": {"verdict_level_rps": 500, "max_error_rate": 0.01,
            "method_qtype": {"getTransaction": "txhash", "getTransactions": "txpage",
                             "getEvents": "events", "getLedgers": "ledgers"}}
```

The p99 target per method and tier is `sla.p99_ns[method_qtype[M]][tier]`
(one copy of each number). `verdict_level_rps` is Q4: 500 is the Standard
tier today. `max_error_rate` 0.01 is a proposal; decide with Q5. A bundle
without the verdict level gives no verdict and a warning.

Verdict rule: `pass = !aborted && error_rate <= max_error_rate && p99 <=
threshold`. `reason` is `aborted`, `errors` or `p99` (first failing check).
The p99 includes failed and timed-out samples (Q5). If Q5 asks for a p99
over successful requests only, B3 cannot compute it from `blaster.json`:
Blaster keeps one histogram for all samples. That needs a Blaster change.

### 4.5 Blended mix versus the old model

`docs/sla-derivation.md` "Known modeling caveat" says each old query run
drove one endpoint at a time. A Blaster run sends one blended stream (60/20/
15/5 at the level). Rewrite that section: contention between methods is now
in the numbers; the request shapes are Blaster's (getEvents limits 1 to
1,000, getLedgers limits 1/5/20/unset, getTransactions 200) and differ from
the "Request shapes" list (10 events, 10 ledgers) unless Q3 forces them;
Blaster and `bench-serve` share the box (D22). The E2E probe family has no
blended measurement.

### 4.6 Site (high level)

- `docs/app.js`: a new renderer for `D.load`, section id `load`: per tier a
  table of methods at the verdict level (p99, target, error rate, pass,
  storage share), a small level table (250, 500, 1000), a getEvents
  archetype table, a per-window p99 line chart from `timeline`, and the
  tx-hash index chunk count next to the cold `getTransaction` row. The
  `queries` renderer stays for old runs.
- `docs/summary.js`: extend `queryVerdicts` (line 699 area) to read
  `load.<tier>.<unit>.verdicts` when `D.load` exists.
- `tests/smoke/smoke.mjs`: count one more section for a run with `load`.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `converter/load.py` | new | Blaster, metrics, `load.json`, verdicts | 230 |
| `converter/convert.py` | modify | Call `build_load`, `SECTION_ORDER`, check entry | 25 |
| `docs/targets.json` | modify | `query_load.blended` | 12 |
| `docs/app.js` | modify | `load` section renderer | 170 |
| `docs/summary.js` | modify | Verdict rows | 80 |
| `tests/smoke/gen-fixtures.py` | modify | One run with mixed load verdicts (under `tests/`, not counted) | 0 |
| `SCHEMA.md`, `docs/sla-derivation.md` | modify | Section and model (not counted) | 0 |
| `converter/tests/test_load.py`, `fixtures.py`, `tests/smoke/smoke.mjs` | new/modify | Tests (not counted) | 0 |

## 6. Tests

`converter/tests/test_load.py`, stdlib `unittest`. Extend B1's
`build_bundle_v2` with `load=True`: it writes `load.json`, `seed.json`,
`<level>rps/blaster.toml`, a `blaster.json` in the `aadc1a1` shape and two
metrics dumps.

| Test | Proves |
|---|---|
| `test_load_section_shape` | Units, tiers, levels, methods; `validate_run` passes |
| `test_blaster_key_mismatch_fails` | Extra or missing key → `BlasterSchemaError` naming the commit |
| `test_unknown_blaster_commit_warns` | Warning, conversion continues |
| `test_seed_mismatch_fails` | `seed` ≠ `rng_seed` |
| `test_error_rate_fails_verdict` | p99 under target, errors 2% → `pass: false`, `reason: errors` |
| `test_aborted_fails_verdict` | `aborted: true` → `reason: aborted` |
| `test_p99_over_target_fails` | Hot and cold thresholds differ (20 ms / 30 ms for getTransaction) |
| `test_no_verdict_without_level` | Bundle without 500 rps → no verdict, warning |
| `test_storage_share_from_deltas` | Ratio from the two dumps, summed over `status` and `store` |
| `test_error_types_kept` | JSON-RPC text keys and counts |
| `test_failed_load_step_skipped` | `status: failed` → warning, no data |
| `test_txhash_index_chunks_attached` | Cold unit carries the ingest step's `txhashIndexChunks` |
| smoke `load` group | `tests/smoke/smoke.mjs` renders the `load` section and verdict rows of the new test run |

## 7. Done when

- The converter reads `blaster.json`, the metrics dumps and `load.json`:
  `test_load_section_shape`, `test_storage_share_from_deltas`.
- Query verdicts exist per method and tier: `test_p99_over_target_fails`.
- A verdict fails on the error rate whatever the p99:
  `test_error_rate_fails_verdict`.
- `targets.json` has the error-rate threshold: `jq
  .query_load.blended.max_error_rate docs/targets.json`.
- The site shows the tx-hash index size next to the cold `getTransaction`
  verdict: smoke `load` group.
- `make test` and `make smoke` pass.

## 8. Verification before push

- `make test`; `make smoke`
- Convert the first real PR 10 bundle; open it with `make serve`.
- `git diff --stat main -- . ':!converter/tests/*' ':!tests/*' ':!*.md'` (D32)

## 9. Risks and open points

- Q4 and Q5 decide `verdict_level_rps` and the meaning of p99. Land with the
  proposed values and change `targets.json` when they are decided.
- Q3: with production shapes, the getEvents and getLedgers targets assume
  other page sizes. Mark the verdicts as provisional in the site until Q3 is
  decided.
- The storage metric names depend on PR 04. Verify on a real dump.
- Size: 520 lines. If the site work grows past 600, split: B3a converter
  and `targets.json` (about 270), B3b site (about 250).
- Q9: a cold `getTransaction` verdict on a 2-chunk index is not production
  scale. The site note next to it states the chunk count.
- Decision log: record `query_load.blended`, the verdict rule and the key
  set check.
