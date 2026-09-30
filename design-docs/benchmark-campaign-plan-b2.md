# B2: The `bundle/**` workflow

| | |
|---|---|
| Branch | `bench-campaign-v2/b2-bundle-workflow` |
| Repository | stellar-experimental/stellar-rpc-benchmarks (base `main` at `3bc7c49`) |
| Depends on | B1 (converter reads the bundle layout). PR 08 (the box pushes `bundles/<id>/` on branch `bundle/<id>`). Land before the first run with `publish=yes`. |
| Implements | D11, D16 (2), D17; spec Section 8 item 1, 10.2 B2 |
| Estimate | about 140 non-test lines: `.github/workflows/bundle.yml` 70, `scripts/publish-bundle.sh` 70 |

## 1. Goal

After this PR, a push of branch `bundle/<id>` to the benchmarks repository
starts a workflow. It converts `bundles/<id>/`, runs `make test` and `make
smoke`, commits `docs/runs/<id>.json` and `docs/runs/index.json` to `main`,
starts the site deploy and deletes the branch. No stellar-rpc job and no
laptop is in the path.

## 2. Scope

- Add `.github/workflows/bundle.yml` (trigger, permissions, gates, deploy,
  branch delete).
- Add `scripts/publish-bundle.sh`: convert, commit, push to `main` with the
  rebuild-on-race loop of `scripts/ingest.sh`.
- Add `converter/tests/test_publish_bundle.py` (runs the script against a
  local bare repository).
- Add a README section "Published bundles" and fix the stale
  `.github/workflows/ingest.yml` line.

## 3. Out of scope (boundaries)

- The converter change: B1. Query verdicts: B3.
- Removal of `scripts/ingest.sh`, `make ingest`, `runner/`: B4. This PR
  copies the push loop from `ingest.sh` (lines 406 to 454), so B4 can delete
  `ingest.sh` without breaking the bundle path.
- The push from the box (token, retries, `bundles/<id>/` path): PR 08.

## 4. Design

### 4.1 Workflows today (verified at `3bc7c49`)

| Workflow | Trigger | Job |
|---|---|---|
| `tests.yml` | `push` to `main`, every `pull_request`; no `paths` filter | `make test`; Node 22 `make smoke` |
| `deploy-pages.yml` | `push` to `main` with `paths: docs/**`; `workflow_dispatch` | Sync `docs/` to `gh-pages` (JamesIves action), `concurrency: gh-pages-sync` |
| `pr-preview.yml` | every `pull_request` | Preview under `gh-pages:/pr-preview/` |
| `runner-go.yml` | `runner/**` | Go vet and test of the runner (B4 removes it) |
| `shellcheck.yml` | `runner/**`, `scripts/**` | `bash -n`, shellcheck on `runner/*.sh scripts/*.sh` |

Nothing reacts to a bundle today (facts A6). A push to `bundle/**` starts no
existing workflow: `tests.yml` and `deploy-pages.yml` filter on `main`.

### 4.2 `bundle.yml`

```yaml
on:
  push:
    branches: ['bundle/**']
permissions:
  contents: write   # commit to main, delete the branch
  actions: write    # dispatch deploy-pages.yml
concurrency: {group: bundle-publish, cancel-in-progress: false}
```

One job `publish`, `ubuntu-latest`, `timeout-minutes: 20`:

1. `ID=${GITHUB_REF_NAME#bundle/}`. Fail unless `ID` matches
   `^[A-Za-z0-9._-]+-[0-9]+$` (`<name>-<GitHub run id>`, spec Section 7.3).
2. `actions/checkout@v4` with `ref: main`.
3. `git fetch origin "$GITHUB_SHA"`, then `git archive FETCH_HEAD
   "bundles/$ID" | tar -x -C "$RUNNER_TEMP"`. Fail when the path is missing.
   The bundle files never enter the `main` tree.
4. `actions/setup-node@v4` (`node-version: "22"`, as `tests.yml`).
5. `scripts/publish-bundle.sh "$RUNNER_TEMP/bundles/$ID" "$ID"` with
   `PUBLISH_GATE="make test && make smoke"`.
6. `gh workflow run deploy-pages.yml --ref main` (`GH_TOKEN:
   ${{ github.token }}`). A push made with `GITHUB_TOKEN` does not start
   `push` workflows, so the `docs/**` trigger of `deploy-pages.yml` does not
   fire. A `workflow_dispatch` made with `GITHUB_TOKEN` does start a run.
7. `git push origin --delete "$GITHUB_REF_NAME"`, only when steps 1 to 6
   pass. On a failure the branch stays, so a re-run can use it. The bundle
   is also in S3 (D31).
8. Summary: `viewer: https://stellar-experimental.github.io/stellar-rpc-benchmarks/?run=<id>`.

`concurrency: bundle-publish` runs one publish at a time, so two bundles do
not race on `docs/runs/index.json`. The push loop still handles a human push
to `main` in between.

Token: the default `GITHUB_TOKEN` with the permissions above. No new secret.
The box's push token (PR 08) is a personal or app token, so its push to
`bundle/**` does start this workflow.

### 4.3 `publish-bundle.sh <bundle dir> <id>`

From `ingest.sh` `--push-main` (lines 319 to 454), without the fetch, the
tarball, the `--local` and `--dry-run` modes and the `metadata.json` body:

```
convert:  python3 converter/convert.py "$BUNDLE" --out-dir docs/runs \
            --source-uri "s3://stellar-rpc-bench/results/$ID/"
gate:     eval "$PUBLISH_GATE"        # empty in tests
commit:   git add docs/runs/$ID.json docs/runs/index.json; git commit -m "runs: add $ID"
push:     git push origin HEAD:main, up to PUBLISH_PUSH_ATTEMPTS (default 5)
```

Checks: HEAD equals `origin/main` before the convert (as `ingest.sh` line
332); `docs/runs/$ID.json` must not exist (a second push of the same id is a
no-op with exit 0, not a failure); `--dataset-kind` is not passed (B1 reads it
from `campaign.json`). On a rejected push: fetch; if `origin/main` moved,
`git reset --hard origin/main` and convert, gate and commit again; else sleep
10 s and retry. Converter warnings (`WARN:` lines) go into the commit body.
The committer is `github-actions[bot]`.

## 5. Files

| File | Change | What | Lines |
|---|---|---|---|
| `.github/workflows/bundle.yml` | new | Trigger, gates, deploy, delete | 70 |
| `scripts/publish-bundle.sh` | new | Convert, commit, push loop | 70 |
| `converter/tests/test_publish_bundle.py` | new | Script test (test file) | 0 |
| `README.md` | modify | "Published bundles"; drop the `ingest.yml` line (not counted) | 0 |

## 6. Tests

`converter/tests/test_publish_bundle.py` (stdlib `unittest`, run by `make
test`). It creates a bare repository as `origin` in a temp dir, clones it
with a copy of `converter/` and `docs/`, and builds a bundle with B1's
`fixtures.build_bundle_v2`. It runs the script with `PUBLISH_GATE=` (empty)
through `subprocess`, never by reading its text.

| Test | Proves | How |
|---|---|---|
| `test_publishes_to_main` | Run JSON and manifest reach `origin/main` in one commit `runs: add <id>` | `git log origin/main`. |
| `test_rebuilds_when_main_moves` | Race recovery | A `pre-receive` hook in the bare repo rejects the first push; a second clone pushes a change to `main` first. |
| `test_same_id_is_noop` | Re-delivery is safe | Run twice; one commit. |
| `test_gate_failure_pushes_nothing` | `make test`/`make smoke` gate | `PUBLISH_GATE=false`. |
| `test_bad_bundle_fails` | Converter failure stops the push | Empty dir. |

`shellcheck.yml` already covers `scripts/*.sh`. actionlint on `bundle.yml`
runs locally (the repository has no actionlint job).

## 7. Done when

- A push to `bundle/<id>` converts, runs `make test` and `make smoke`,
  commits `docs/runs/<id>.json` to `main` and deletes the branch. Check: push
  a test bundle branch (a B1 test bundle under `bundles/test-1/`); the
  workflow is green; `git ls-remote origin 'bundle/*'` is empty;
  `docs/runs/test-1.json` is on `main`; the site shows `?run=test-1`. Then
  revert the commit.
- `make test` passes with `test_publish_bundle.py`.

## 8. Verification before push

- `make test`; `make smoke`
- `shellcheck scripts/publish-bundle.sh`; `bash -n scripts/publish-bundle.sh`
- `actionlint .github/workflows/bundle.yml`
- `git diff --stat main -- . ':!converter/tests/*' ':!tests/*' ':!*.md'` (D32)

## 9. Risks and open points

- Branch protection on `main` is not known (GitHub state not checked, HTTP
  403). If `main` needs reviews or status checks, `GITHUB_TOKEN` cannot push.
  Then add an app token secret with a bypass, or change the rule. Check the
  settings first.
- The default `GITHUB_TOKEN` permission of the repository may be read-only.
  The `permissions:` block raises it for this workflow only if the
  organization allows it. Check.
- The deploy runs by dispatch, not by the `docs/**` push. If the dispatch
  fails, the site is stale until the next `docs/` push. Step 6 fails the job,
  so the branch stays and the failure shows.
- The bundle branch holds test data and large files (`bench-serve.log`).
  They stay in git history after the branch delete until GitHub collects
  them. Accept, or let the box leave out large files (PR 08 risk).
- Decision log: record the dispatch of `deploy-pages.yml`, the
  `bundle-publish` concurrency group and the copy of the push loop into
  `publish-bundle.sh`.
