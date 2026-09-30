# B4: Remove the benchmarks runner, its bootstrap and `scripts/ingest.sh`

| | |
|---|---|
| Branch | `bench-campaign-v2/b4-remove-runner` |
| Repository | stellar-experimental/stellar-rpc-benchmarks (base `main` after B2) |
| Depends on | PR 08 (box bootstrap port in stellar-rpc), PR 09 (workflow on EC2; one campaign ran end to end), B2 (`scripts/publish-bundle.sh` replaces the `ingest.sh` push loop) |
| Implements | D15, D16 (4), D32 (deletions do not count); spec Section 8 item 5, Section 9 (last row), 10.2 B4 |
| Estimate | about 15 non-test lines added or modified (Makefile help text, `shellcheck.yml` paths); this is the D32 count. Removed, not counted: about 4,700 non-test lines (`runner/` 4,223 without tests and `*.md`, `scripts/ingest.sh` 454, `runner-go.yml` 38). |

## 1. Goal

After this PR, the benchmarks repository holds only the judge: the
converter, the targets, the site and the bundle workflow. The Go campaign
runner, `runner/bootstrap.sh`, `scripts/ingest.sh`, `make ingest` and the
runner CI are gone. stellar-rpc runs every campaign (D8, D15).

## 2. Scope

- Delete `runner/` (44 tracked files: `README.md`, `bootstrap.sh`,
  `example-campaign.toml`, `go.mod`, `go.sum`, `cmd/campaign/`,
  `internal/{bundle,config,plan,preflight,publish,run,targets}/`).
- Delete `scripts/ingest.sh` and `.github/workflows/runner-go.yml`.
- Change `Makefile`: remove `ingest`, `runner-build`, `runner-test` and their
  help lines.
- Change `.github/workflows/shellcheck.yml`: remove the `runner/**` paths and
  `runner/*.sh`, `runner/cmd/campaign/testdata/*.sh` from both steps.
- Rewrite the README sections that describe the runner and `ingest.sh`.

## 3. Out of scope (boundaries)

- The converter's legacy layouts (`campaign`, `pubnet`, `synthetic`,
  `metadata.json`, `invocation.json`, CSV): kept. Committed runs came from
  them, and a re-conversion must stay possible.
- `docs/targets.json`: kept. Only the runner's copy of the reader
  (`runner/internal/targets`) goes.
- `scripts/publish-bundle.sh` and `bundle.yml`: B2, kept.
- stellar-rpc files: none. The bootstrap port is PR 08.

## 4. Design

### 4.1 What reads what today (verified at `3bc7c49`)

| Item | Used by | After B4 |
|---|---|---|
| `runner/` (Go module, `go 1.26`) | devbox campaigns; `runner-go.yml`; `shellcheck.yml` | gone |
| `runner/bootstrap.sh` (174 lines) | devbox; stellar-rpc #16 `run-campaign.sh` (never merged) | gone; ported as stellar-rpc `perf-eval/bench-campaign/box-bootstrap.sh` (PR 08) |
| `scripts/ingest.sh` (454 lines) | `make ingest`; stellar-rpc #20 `ingest-results-site.sh` (never merged) | gone; B2 `scripts/publish-bundle.sh` carries the push loop |
| `runner-go.yml` | `runner/**` changes | gone |
| `shellcheck.yml` | `runner/**`, `scripts/**` | `scripts/**` only |
| `tests.yml`, `deploy-pages.yml`, `pr-preview.yml` | converter and site | unchanged |

`converter/convert.py` does not import runner code. It reads the bundle
files that the runner wrote (`metadata.json`, `plan.json`, `leg.json`,
`invocation.json`) as data; those readers stay.

### 4.2 README changes

Sections of `README.md` (line numbers at `3bc7c49`):

- Line 13 ("driven by the config-driven runner in `runner/`"): say that
  stellar-rpc's `Bench campaign` workflow runs campaigns on EC2 and pushes
  each `publish=yes` bundle to `bundle/<id>`.
- "Run a campaign" (line 55): replace with a short pointer to the stellar-rpc
  workflow (inputs of spec Section 5) and to the S3 prefix
  `s3://stellar-rpc-bench/results/<id>/`.
- "Add a run" (line 95): keep `make convert` (line 162). Remove the `make
  ingest` and `scripts/ingest.sh` parts (lines 97 to 160). State the manual
  path: download the bundle from S3, run `make convert`, open a PR.
- "Automated ingest (stellar-rpc's `bench-campaign.yml`)" (line 224):
  replace with "Published bundles" from B2 (if B2 did not add it already).
- "Repo layout" (line 265): remove `runner/`, `ingest.sh`, `runner-go.yml`
  and the stale `ingest.yml` row (line 274); add `bundle.yml` and
  `publish-bundle.sh`.
- `SCHEMA.md` lines 419 to 420: the `campaign` layout was produced by the
  former runner; point to this README history, not to `runner/README.md`.
- `Makefile` header comment line 2 and help text lines 21 to 23: remove the
  `ingest` lines.

### 4.3 Order and safety

1. Merge only after one stellar-rpc campaign ran on EC2 with the ported
   bootstrap (PR 09 "Done when") and one `publish=yes` bundle reached
   `main` through B2.
2. Search once more before the merge: `git grep -n -e 'runner/' -e
   'ingest.sh' -e 'make ingest' -e 'runner-build' -e 'runner-test'` must
   only match history notes.
3. No tag is needed to find the runner later: the last commit before B4
   holds it. Name that commit in the PR description.

## 5. Files

| File | Change | What | Lines (added) |
|---|---|---|---|
| `runner/**` (44 files) | delete | Go runner, bootstrap, example config | 0 (−4,223 non-test) |
| `scripts/ingest.sh` | delete | Laptop and CI ingest | 0 (−454) |
| `.github/workflows/runner-go.yml` | delete | Runner CI | 0 (−38) |
| `.github/workflows/shellcheck.yml` | modify | Drop runner paths | 4 |
| `Makefile` | modify | Drop `ingest`, `runner-build`, `runner-test` | 5 |
| `README.md`, `SCHEMA.md` | modify | Sections of 4.2 (not counted) | 0 |

## 6. Tests

No new test. The removal must leave these green:

| Check | What it proves | How |
|---|---|---|
| `make test` | Converter tests do not depend on `runner/` | `python3 -m unittest discover converter/tests` |
| `make smoke` | Site renders every committed run | Node 22 |
| `make help` | Help text lists no removed target | Read the output |
| `shellcheck.yml` | Workflow runs on `scripts/*.sh` only | PR run |
| `test_publish_bundle.py` (B2) | The bundle path does not use `ingest.sh` | `make test` |

## 7. Done when

- `runner/`, `runner/bootstrap.sh` and `scripts/ingest.sh` are gone:
  `test ! -e runner && test ! -e scripts/ingest.sh`.
- `runner-go.yml` is gone and `shellcheck.yml` has no runner path:
  `git grep -n runner .github/workflows` is empty.
- The README describes the stellar-rpc workflow and the bundle workflow:
  `git grep -n -e 'make ingest' -e 'scripts/ingest.sh' README.md` is empty.
- `make test` and `make smoke` pass.

## 8. Verification before push

- `make test`; `make smoke`; `make help`
- `shellcheck scripts/*.sh`
- `actionlint .github/workflows/*.yml` (local)
- `git grep -n -e 'runner/' -e 'ingest.sh' -e 'make ingest'`
- `git diff --numstat main -- . ':!converter/tests/*' ':!tests/*' ':!*.md'`;
  sum the first column (D32: pure deletions do not count)

## 9. Risks and open points

- D32 counts the first column of `git diff --numstat` only, so the about
  4,700 deleted lines do not count, and the PR is at about 15 lines. The
  reviewer still reads the list of deleted files: check it against 4.1.
- Pubnet campaigns (`dataset.kind: pubnet`) ran only through the runner.
  After B4 nothing produces a new pubnet bundle. stellar-rpc campaigns are
  synthetic (`profiles[].datasetKind`). Confirm that no pubnet run is
  planned, or keep this PR until one exists.
- A developer who still has a devbox with `runner/` can keep running it from
  the old commit; its bundles no longer have a push path (`ingest.sh` is
  gone). `make convert` still converts them.
- Merge order: B4 after PR 09 has run. If B4 lands first, a stellar-rpc
  branch that still clones the benchmarks runner (#16 layout) breaks; the v2
  branches do not clone it.
