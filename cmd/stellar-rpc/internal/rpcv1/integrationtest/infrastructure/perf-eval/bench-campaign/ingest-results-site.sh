#!/usr/bin/env bash
# Folds a passing campaign into the results site: fetch the tarball the box
# uploaded, clone stellar-rpc-benchmarks at the ref the box ran, run its
# ingest script (convert, gate on the repo's tests, push docs/runs/<id>.json to
# main; that push IS the site deploy). Producer and consumer must run the same
# benchmarks code: main's older converter would silently drop the sections a
# feature ref added. The site publishes from main only (--push-main refuses a
# HEAD that is not origin/main), so another ref converts with --local on a
# local run/<id> branch, nothing pushed; re-ingest by hand once it merges.
# Best-effort: an ingest failure must never turn a passing campaign red, so
# every path exits 0 and reports through ingest_state (ingested|skipped|failed)
# and ingest_reason, which go on the Slack card.
# Env: TARBALL_KEY, RESULTS_URI, PUSH_TOKEN (write access to the benchmarks
# repo; empty degrades to a warning), BUCKET, BENCH_REPO_REF (default main).
# Outputs to GITHUB_OUTPUT (stdout when unset): ingest_state, ingest_reason
# always; viewer_url on a live publish; run_json when the converted JSON exists.
set -euo pipefail

BENCH_REPO_REF="${BENCH_REPO_REF:-main}"
# Written on every path, always before the exit; the reason is one line.
emit() {
  {
    echo "ingest_state=$1"
    echo "ingest_reason=$(printf '%s' "${2:-}" | tr -d '\r\n')"
  } >> "${GITHUB_OUTPUT:-/dev/stdout}"
}
# Names the gate that rejected the run, from the ingest log, so the card says
# what broke instead of "see the run".
classify_failure() {
  FAIL_LINE=""
  if grep -q 'SMOKE SUMMARY' /tmp/ingest.log 2>/dev/null \
     && ! grep -q 'SMOKE SUMMARY: [0-9]* passed, 0 failed' /tmp/ingest.log 2>/dev/null; then
    FAIL_LINE=$( (grep -m1 'FAIL \[' /tmp/ingest.log | sed 's/^[[:space:]]*//') || true )
    [ -n "$FAIL_LINE" ] || FAIL_LINE="no FAIL line in the log"
    # Bash substring, not cut -c: GNU cut counts bytes and would split the em
    # dash the smoke test prints.
    echo "viewer smoke gate failed (${FAIL_LINE:0:120})"
  elif grep -q 'make: \*\*\* \[Makefile:.*test\]' /tmp/ingest.log 2>/dev/null; then
    echo "converter tests failed"
  elif grep -q 'converter failed' /tmp/ingest.log 2>/dev/null; then
    echo "converter failed"
  elif grep -q 'already exists (run already ingested)' /tmp/ingest.log 2>/dev/null; then
    echo "run already ingested"
  else
    echo "ingest.sh exited non-zero (see step log)"
  fi
}
if [ -z "$PUSH_TOKEN" ]; then
  echo "::warning::BENCHMARKS_PUSH_TOKEN is unset; run not ingested into the results site"
  emit skipped "BENCHMARKS_PUSH_TOKEN unset"
  exit 0
fi
if ! aws s3 cp "s3://$BUCKET/$TARBALL_KEY" /tmp/bench-bundle.tgz; then
  echo "::warning::tarball download failed; run not ingested into the results site"
  emit failed "tarball download failed"
  exit 0
fi
# Full clone: ingest.sh pushes HEAD:main, which a shallow clone's server-side
# check can reject. The token rides in the URL; Actions masks it in logs.
if ! git clone "https://x-access-token:${PUSH_TOKEN}@github.com/stellar-experimental/stellar-rpc-benchmarks.git" /tmp/benchmarks; then
  echo "::warning::benchmarks repo clone failed; run not ingested into the results site"
  emit failed "benchmarks clone failed (ref $BENCH_REPO_REF)"
  exit 0
fi
# Checkout, not clone --branch: --branch rejects a commit SHA. `checkout main`
# leaves HEAD at origin/main for --push-main; --local works off a detached HEAD.
if ! git -C /tmp/benchmarks checkout --quiet "$BENCH_REPO_REF"; then
  echo "::warning::benchmarks ref $BENCH_REPO_REF not found; run not ingested into the results site"
  emit failed "benchmarks ref $BENCH_REPO_REF not found"
  exit 0
fi
git -C /tmp/benchmarks config user.name "github-actions[bot]"
git -C /tmp/benchmarks config user.email "41898282+github-actions[bot]@users.noreply.github.com"
MODE_FLAG=--push-main
if [ "$BENCH_REPO_REF" != "main" ]; then
  MODE_FLAG=--local
fi
# Campaign datasets are the synthetic apply-load packs, hence the hard-coded
# kind; revisit if a pubnet campaign ever runs through this workflow.
if (cd /tmp/benchmarks && scripts/ingest.sh /tmp/bench-bundle.tgz \
      --dataset-kind synthetic "$MODE_FLAG" \
      -- --source-uri "$RESULTS_URI") 2>&1 | tee /tmp/ingest.log; then
  # The notification reads p99s and the build commit from the run JSON just
  # committed. --push-main names the live run in a `viewer:` line; --local
  # prints none, so read the id off the commit subject (`runs: add <id>`).
  VIEWER=""
  SITE_RUN_ID=""
  if [ "$MODE_FLAG" = "--push-main" ]; then
    VIEWER=$( (grep '^viewer: ' /tmp/ingest.log | tail -1 | cut -d' ' -f2) || true )
    echo "viewer_url=$VIEWER" >> "${GITHUB_OUTPUT:-/dev/stdout}"
    SITE_RUN_ID="${VIEWER##*run=}"
  else
    SITE_RUN_ID=$( (sed -n 's/^.*runs: add \(.*\)$/\1/p' /tmp/ingest.log | tail -1) || true )
  fi
  if [ -n "$SITE_RUN_ID" ] && [ -f "/tmp/benchmarks/docs/runs/$SITE_RUN_ID.json" ]; then
    echo "run_json=/tmp/benchmarks/docs/runs/$SITE_RUN_ID.json" >> "${GITHUB_OUTPUT:-/dev/stdout}"
  fi
  if [ "$MODE_FLAG" = "--push-main" ]; then
    emit ingested "published to the results site"
    echo "ingested; viewer: $VIEWER"
  else
    echo "::warning::benchmarks_ref $BENCH_REPO_REF is not main; the run was converted on a local run/ branch but not published to the results site"
    emit skipped "benchmarks_ref $BENCH_REPO_REF is not main — site publishes from main only; merge it, then re-ingest by hand"
  fi
else
  INGEST_REASON=$(classify_failure)
  echo "::warning::results-site ingest failed: $INGEST_REASON; the notification falls back to the raw results URI"
  emit failed "$INGEST_REASON"
fi
