#!/usr/bin/env bash
# Folds a passing campaign into the results site: fetch the tarball the box
# uploaded, clone stellar-rpc-benchmarks at the ref the box ran, run its
# ingest script (convert, gate on the repo's tests, push docs/runs/<id>.json to
# main; that push IS the site deploy). The converter must support the bundle's
# data format; Git commit equality is not a compatibility check.
# The site publishes from main only (--push-main refuses a
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
EMITTED=false
# Written on every path, always before the exit; the reason is one line.
emit() {
  {
    echo "ingest_state=$1"
    echo "ingest_reason=$(printf '%s' "${2:-}" | tr -d '\r\n')"
  } >> "${GITHUB_OUTPUT:-/dev/stdout}"
  EMITTED=true
}
# Unexpected local failures also reach the notification as a failed ingest.
finish() {
  if [ "$EMITTED" != true ]; then
    echo "::warning::results-site ingest stopped unexpectedly; see step log"
    emit failed "ingest stopped unexpectedly (see step log)" || true
  fi
  exit 0
}
trap finish EXIT
WORK=$(mktemp -d "${TMPDIR:-/tmp}/campaign-ingest-XXXXXX")
# Names the gate that rejected the run, from the ingest log, so the card says
# what broke instead of "see the run".
classify_failure() {
  FAIL_LINE=""
  if grep -q 'SMOKE SUMMARY' "$WORK/ingest.log" 2>/dev/null \
     && ! grep -q 'SMOKE SUMMARY: [0-9]* passed, 0 failed' "$WORK/ingest.log" 2>/dev/null; then
    FAIL_LINE=$( (grep -m1 'FAIL \[' "$WORK/ingest.log" | sed 's/^[[:space:]]*//') || true )
    [ -n "$FAIL_LINE" ] || FAIL_LINE="no FAIL line in the log"
    # Bash substring, not cut -c: GNU cut counts bytes and would split the em
    # dash the smoke test prints.
    echo "viewer smoke gate failed (${FAIL_LINE:0:120})"
  elif grep -q 'make: \*\*\* \[Makefile:.*test\]' "$WORK/ingest.log" 2>/dev/null; then
    echo "converter tests failed"
  elif grep -q 'converter failed' "$WORK/ingest.log" 2>/dev/null; then
    echo "converter failed"
  elif grep -q 'already exists (run already ingested)' "$WORK/ingest.log" 2>/dev/null; then
    echo "run already ingested"
  else
    echo "ingest.sh exited non-zero (see step log)"
  fi
}
if [ -z "${PUSH_TOKEN:-}" ]; then
  echo "::warning::BENCHMARKS_PUSH_TOKEN is unset; run not ingested into the results site"
  emit skipped "BENCHMARKS_PUSH_TOKEN unset"
  exit 0
fi
if ! aws s3 cp "s3://$BUCKET/$TARBALL_KEY" "$WORK/bench-bundle.tgz"; then
  echo "::warning::tarball download failed; run not ingested into the results site"
  emit failed "tarball download failed"
  exit 0
fi
# Full clone: ingest.sh pushes HEAD:main, which a shallow clone's server-side
# check can reject. The token rides in the URL; Actions masks it in logs.
if ! git clone "https://x-access-token:${PUSH_TOKEN}@github.com/stellar-experimental/stellar-rpc-benchmarks.git" "$WORK/benchmarks"; then
  echo "::warning::benchmarks repo clone failed; run not ingested into the results site"
  emit failed "benchmarks clone failed (ref $BENCH_REPO_REF)"
  exit 0
fi
# Checkout, not clone --branch: --branch rejects a commit SHA. `checkout main`
# leaves HEAD at origin/main for --push-main; --local works off a detached HEAD.
if ! git -C "$WORK/benchmarks" checkout --quiet "$BENCH_REPO_REF"; then
  echo "::warning::benchmarks ref $BENCH_REPO_REF not found; run not ingested into the results site"
  emit failed "benchmarks ref $BENCH_REPO_REF not found"
  exit 0
fi
git -C "$WORK/benchmarks" config user.name "github-actions[bot]"
git -C "$WORK/benchmarks" config user.email "41898282+github-actions[bot]@users.noreply.github.com"
echo "converter checkout: $(git -C "$WORK/benchmarks" rev-parse HEAD)"
MODE_FLAG=--push-main
if [ "$BENCH_REPO_REF" != "main" ]; then
  MODE_FLAG=--local
fi
# Campaign datasets are the synthetic apply-load packs, hence the hard-coded
# kind; revisit if a pubnet campaign ever runs through this workflow.
if (cd "$WORK/benchmarks" && scripts/ingest.sh "$WORK/bench-bundle.tgz" \
      --dataset-kind synthetic "$MODE_FLAG" \
      -- --source-uri "$RESULTS_URI") 2>&1 | tee "$WORK/ingest.log"; then
  # The notification reads p99s and the build commit from the run JSON just
  # committed. --push-main names the live run in a `viewer:` line; --local
  # prints none, so read the id off the commit subject (`runs: add <id>`).
  VIEWER=""
  SITE_RUN_ID=""
  if [ "$MODE_FLAG" = "--push-main" ]; then
    VIEWER=$( (grep '^viewer: ' "$WORK/ingest.log" | tail -1 | cut -d' ' -f2) || true )
    if [ -z "$VIEWER" ]; then
      emit failed "ingest succeeded without a viewer URL; publication unconfirmed"
      exit 0
    fi
    SITE_RUN_ID="${VIEWER##*run=}"
  else
    SITE_RUN_ID=$( (sed -n 's/^.*runs: add \(.*\)$/\1/p' "$WORK/ingest.log" | tail -1) || true )
  fi
  if [ -n "$SITE_RUN_ID" ] && [ -f "$WORK/benchmarks/docs/runs/$SITE_RUN_ID.json" ]; then
    echo "run_json=$WORK/benchmarks/docs/runs/$SITE_RUN_ID.json" >> "${GITHUB_OUTPUT:-/dev/stdout}"
  fi
  if [ "$MODE_FLAG" = "--push-main" ]; then
    echo "viewer_url=$VIEWER" >> "${GITHUB_OUTPUT:-/dev/stdout}"
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
