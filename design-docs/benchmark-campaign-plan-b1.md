# B1: Converter reads the new bundle layout and `results.json`

| | |
|---|---|
| Branch | `bench-campaign-v2/b1-converter-bundle` |
| Repository | stellar-experimental/stellar-rpc-benchmarks (base `main` at `3bc7c49`) |
| Depends on | PR 03 (`results.json` schema), PR 06 (`campaign.json` schema, spec 7.3; a real bundle needs PR 07), D34 (measurement names `<group>.<row>`), D29 (`schemaVersion`) |
| Implements | D9, D12, D20; spec Sections 7.2, 7.3, 8 item 2, 10.2 B1 |
| Estimate | about 300 non-test lines: `converter/bundle.py` 210 (new), `converter/convert.py` 90 (layout switch, argument change, builder split). `tests/smoke/gen-fixtures.py` (+30) is under `tests/`, so D32 does not count it. |

## 1. Goal

After this PR, `converter/convert.py` converts a results bundle in the
Section 7 layout (`campaign.json` plus `steps/<name>/results.json`). It
reads fields, not directory names. It takes the dataset kind, profile, run
and close interval from `campaign.json`, and it derives the phase from the
close interval. The run JSON it writes has the same `ingest_cold` and
`ingest_hot` shape as today, so `docs/app.js` and `docs/summary.js` render it
unchanged. Legacy layouts convert as before.

## 2. Scope

- Add `converter/bundle.py`: load and check `campaign.json` and each
  `results.json`, and turn measurements into the row dicts that the current
  builders take.
- Change `converter/convert.py`: detect the new layout first; make
  `--dataset-kind` optional for it; split `build_ingest_cold` and
  `build_ingest_hot` into a reader part and a core part.
- Add a test bundle writer and `converter/tests/test_bundle.py`.
- Add one new-layout run to `tests/smoke/gen-fixtures.py`.
- Update `SCHEMA.md` (Inputs section) for the new layout.

## 3. Out of scope (boundaries)

- `load.json`, `blaster.json`, metrics dumps and query verdicts: B3.
- A `freeze` section in the run JSON: not in this PR. The converter skips
  `freeze` steps with a warning (D23 sets no target). Add a section in a
  later change if the site needs it.
- The `bundle/**` workflow: B2. Removal of `scripts/ingest.sh` and `runner/`:
  B4.
- Removal of the CSV reader: never. Committed runs were converted from CSV
  bundles, and a re-conversion must stay possible. Only stellar-rpc stops
  writing CSV (PR 11).

## 4. Design

### 4.1 Layout detection

`detect_layout(names)` today returns `synthetic`, `campaign` or `pubnet` from
directory names (`_CAMPAIGN_RUN` regex, `convert.py` line 127). Add a check
before `subdirs()`: when `<results_dir>/campaign.json` exists and
`<results_dir>/steps` is a directory, the layout is `bundle`. `convert()`
then calls `bundle.convert_bundle(args)` and returns its result. The other
layouts keep their path.

### 4.2 `bundle.py`

```python
def load_campaign(root) -> dict          # fails on schemaVersion != 1
def load_results(root, step) -> dict     # fails on schemaVersion != 1 or command != step["kind"]
def result_rows(results) -> tuple[dict, int | None]   # ({group: {row: csv_row}}, peak_rss_bytes)
def unit_id(step) -> str                 # f"{step['profile']}-c{step['startChunk']}"
def convert_bundle(args) -> dict
```

`result_rows` maps each `summary` measurement to the dict that `read_csv`
returns today: `{"n": count, "n_items": items, "total_ns", "p50_ns",
"p90_ns", "p99_ns", "max_ns"}`. The name splits at the first dot (D34):
`driver.ingest_total` → group `driver`, row `ingest_total`. All values are
integers, and `unit` is `ns`, `bytes` or `count` (spec 7.2 rule 1, D12). A
`summary` must have `unit: ns`. Any other unit, or a value that is not an
integer, fails with the measurement name. The `value` measurement
`driver.peak_rss` with `unit: bytes` returns as the second value. Any other
`value` measurement is kept out with a warning.

Groups map to the current run JSON keys: `driver` → `driver`, `hot` →
`phases`, `ledgers`, `txhash`, `events` → `files.<group>`.

### 4.3 Field sources (`campaign.json`, spec Section 7.3)

| Run JSON field | Source | Today |
|---|---|---|
| `run_id` | `id` | `metadata.json` `run_id` |
| `run_date` | `startedAt[:10]` | `metadata.json` `started_at` |
| `run_name` | `inputs.name` | `campaign.name` |
| `dataset.kind` | `profiles[].datasetKind` (all equal, else fail) | `--dataset-kind` |
| unit order | `profiles[]` order, then chunk | `metadata.json` `datasets[]` |
| unit id | `<profile>-c<startChunk>` from `steps[]` | directory name regex |
| rep | `steps[].run` | directory name regex |
| step dir | `steps[].path` | directory name |
| `campaign.close_interval_ns` | `parse_go_duration(inputs.closeInterval)` | `metadata.json` or `invocation.json` flags |
| `campaign.phase` | `match_phase(close_interval_ns)` | same, plus `query_phase` |
| `build.commit`, `build.version` | `commit`; `results.json` `binary.commit`, `binary.version` | `invocation.json` `binary.commitHash` |
| `machine`, `hardware` | `machine.instanceType`, `cpus`, `memoryGiB` | `machine-metadata.txt`, `metadata.json` |
| `campaign.config` | `inputs`, `load.packs_prefix`, `runnerCommit`, `blasterCommit` | `metadata.json` `campaign` |

The unit id keeps the `<dataset>-c<chunk>` form, because `query_profile`
(`_PROFILE_TAIL` in `convert.py`), `summary.js` dataset sizes and `app.js`
labels depend on it. `--dataset-kind` becomes optional in `main()`: required for
the legacy layouts (checked in `convert()`), and a failure when it differs
from `campaign.json`. The phase needs no `query_phase`: the close interval
always matches a phase block time (2s, 1s, 600ms; D10).

`resolve_binary` keeps its mismatch warning: each `results.json`
`binary.commit` must equal `campaign.json` `commit`.

### 4.4 Step selection

Only steps with `kind` in `ingest-cold`, `ingest-hot` and `status: ok`
convert. A step with `failed`, `crashed`, `pending` or `skipped` gives one
warning (`step <name> is <status>: <error>`) and no data. `load` and
`freeze` steps give one warning each. A bundle with no convertible step
fails. The current CSV path converts failed runs with a warning
(`load_invocations`); the new path does not, because a `results.json` with
`status: failed` is partial by definition (spec Section 7.2 rule 7).

### 4.5 Builder split

`build_ingest_cold(results_dir, layout, unit, reps, vocab, counts)` and
`build_ingest_hot(...)` read CSVs and aggregate. Split each into
`_cold_from_rows(drivers, files_rows, peaks, counts)` and
`_hot_from_rows(drivers, hots, peaks, counts)`. The CSV path reads the rows
and calls them. `bundle.py` builds the rows with `result_rows` and calls
them. `vocab` is `new` for the bundle layout (`backfill_wall`, `run_wall`).

Counts: `unit_counts` reads `n_items` of `ledgers_total`, `txhash_total` and
`events_total` from the cold driver rows. The bundle path reads `items` of
the same measurements. The hot `ledgers_per_s` must use `driver.run_wall`
`items` (spec 7.2 rule 4), not the cold ledger count: `run_wall` covers the paced ledgers only
(D21), and a hot dataset of 20,000 ledgers with 10,000 paced ledgers would
double the rate. The CSV path keeps its formula (its hot runs pace every
ledger).

### 4.6 `SCHEMA.md`

In "Inputs — result-bundle layouts & manifests", add the `bundle` layout:
`campaign.json` at the root, `steps/<name>/results.json`, the field table of
4.3, D34 names, the integer units `ns`, `bytes` and `count`, `peak_rss` →
`peak_rss_bytes`, step selection. In "Top level", state that
`campaign.config` for this layout holds `inputs`, `load.packs_prefix`,
`runnerCommit` and `blasterCommit`. Keep the run JSON
`schema_version` at 1: the output shape does not change.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `converter/bundle.py` | new | Load, check, map measurements, convert | 210 |
| `converter/convert.py` | modify | Layout switch, optional `--dataset-kind`, builder split | 90 |
| `tests/smoke/gen-fixtures.py` | modify | One new-layout run for the viewer smoke test (not counted) | 0 |
| `converter/tests/fixtures.py` | modify | `build_bundle_v2(root, ...)` (test file) | 0 |
| `converter/tests/test_bundle.py` | new | Tests (test file) | 0 |
| `SCHEMA.md` | modify | New layout (not counted) | 0 |

## 6. Tests

Stdlib `unittest`, run by `make test` (`python3 -m unittest discover
converter/tests`). Add `build_bundle_v2(root, profiles, reps, close_interval,
statuses, paced_ledgers)` to `converter/tests/fixtures.py`. It writes a
`campaign.json` and one `results.json` per step, with values derived from the
existing `_cold_driver`, `_cold_files`, `_hot_driver` and `_hot_phases` rows,
so the old and new paths can be compared. Reuse `run_convert` from
`test_campaign.py` (pass `dataset_kind=None`).

| Test | Proves | How |
|---|---|---|
| `test_detects_bundle_layout` | `campaign.json` + `steps/` selects the new path | `build_bundle_v2`; check `campaign.config.inputs`. |
| `test_same_output_as_csv_path` | Core builders give equal `ingest_cold` and `ingest_hot` | Same rows through `build_campaign_bundle` and `build_bundle_v2`; compare sections. |
| `test_step_path_not_parsed` | Step dirs come from `steps[].path` | Name dirs `steps/x1`, `steps/x2`. |
| `test_dataset_kind_from_campaign` | No `--dataset-kind` needed | `dataset_kind=None`. |
| `test_dataset_kind_mismatch_fails` | Caller value must agree | `dataset_kind="pubnet"`, `SystemExit`. |
| `test_phase_from_close_interval` | 2s → 1, 1s → 2, 600ms → 3 | Three bundles. |
| `test_peak_rss_value` | `driver.peak_rss` bytes → `peak_rss_bytes` V | Value 412000000. |
| `test_units_checked` | `ns`, `bytes`, `count` pass; `ms` and a float value fail with the name | Measurement units and values. |
| `test_hot_rate_uses_run_wall_items` | `ledgers_per_s` over paced ledgers | `paced_ledgers=100`, cold 20,000 ledgers. |
| `test_non_ok_steps_skipped` | `failed`, `crashed`, `skipped` give a warning and no data | `statuses={...}`. |
| `test_load_and_freeze_steps_skipped` | Warning, no section | Steps of kind `load`, `freeze`. |
| `test_schema_version_unsupported` | Named failure | `schemaVersion: 2` in each file. |
| `test_command_mismatch_fails` | `results.json` `command` equals step `kind` | Swap one. |
| `test_commit_mismatch_warns` | `binary.commit` versus `commit` | Change one commit. |
| existing `test_campaign.py`, `test_phase.py`, `test_convert.py`, `test_golden.py`, `test_queries_rps.py` | Legacy layouts unchanged | Run unchanged. |

## 7. Done when

- The converter converts a Section 7 bundle with ingest steps into a run
  JSON that `validate_run` accepts: `test_detects_bundle_layout`.
- It reads no directory name: `test_step_path_not_parsed`.
- It takes the dataset kind from `campaign.json`:
  `test_dataset_kind_from_campaign`.
- It derives the phase from the close interval:
  `test_phase_from_close_interval`.
- `make test` and `make smoke` pass, the smoke test including one
  new-layout run.

## 8. Verification before push

- `make test`
- `make smoke` (Node 22, as `tests.yml`)
- `python3 converter/convert.py <bundle from a real PR 07 local run> --out-dir /tmp/out`
  and open the result with `make serve`.
- `git diff --numstat main -- . ':!converter/tests/*' ':!tests/*' ':!*.md'`; sum the first column (D32).

## 9. Risks and open points

- PR 03 must emit the names of D34 (`<group>.<row>`) and the fields of spec
  Section 7.2. Check the PR 03 README before the merge. If PR 03 changes a
  name, `result_rows` and the test bundle writer change with it.
- `n` in the CSV excludes zero-duration samples except `pace_lag` (facts).
  `count` in `results.json` must have the same meaning, or
  `test_same_output_as_csv_path` fails. Confirm with PR 03.
- `index_rebuild` and other rows that PR 03 may drop: the core builders
  iterate over the rows present, so a missing row only removes a key.
- The site may show a skipped `sac-6000` second chunk (Q7) as a missing unit.
  Check it in the smoke run.
- Decision log (benchmarks side, or stellar-rpc decision log D12 note):
  record that the bundle path converts `ok` steps only, and that the hot
  rate uses `run_wall` items.
