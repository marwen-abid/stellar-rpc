#!/usr/bin/env bash
#
# Full-history benchmark driver for CI and local use: generate the
# deterministic synthetic dataset, benchmark a baseline build and a candidate
# build against it several times each — interleaved, so machine noise hits
# both sides equally — and gate on `bench-compare`'s verdict.
#
# Usage:
#   CANDIDATE_BIN=bin/stellar-rpc BASELINE_BIN=bin/stellar-rpc-base \
#       ./scripts/bench-ci/run.sh
#
# Environment:
#   CANDIDATE_BIN  stellar-rpc build under test (required)
#   BASELINE_BIN   stellar-rpc build to compare against (default: the
#                  candidate itself — a self-comparison that measures the
#                  machine's noise floor)
#   WORKDIR        scratch root for datasets, artifacts and CSVs
#                  (default: a fresh temp dir)
#   RUNS           benchmark invocations per side (default 6: bench-compare
#                  judges each metric at the Bonferroni-corrected level
#                  0.05/m, and with ~20 gated metrics the smallest exact-
#                  permutation p, 1/C(2*RUNS,RUNS), only clears that from
#                  6 runs per side up)
#   METRICS_FILE   gated-metric list (default: metrics.csv next to this
#                  script)
#   SUMMARY_FILE   Markdown comparison table is appended here (optional;
#                  point it at $GITHUB_STEP_SUMMARY in CI)
#   SKIP_COMPARE   set to 1 to benchmark only the candidate and skip the
#                  gate (the nightly data-collection mode)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CANDIDATE_BIN="${CANDIDATE_BIN:?set CANDIDATE_BIN to the stellar-rpc build under test}"
BASELINE_BIN="${BASELINE_BIN:-$CANDIDATE_BIN}"
WORKDIR="${WORKDIR:-$(mktemp -d)}"
mkdir -p "$WORKDIR"
RUNS="${RUNS:-6}"
METRICS_FILE="${METRICS_FILE:-$SCRIPT_DIR/metrics.csv}"
SUMMARY_FILE="${SUMMARY_FILE:-}"
SKIP_COMPARE="${SKIP_COMPARE:-0}"

# The defined dataset and workload. Everything below is fixed so that every
# run of this script measures the same work.
CHUNK=0
HOT_LEDGERS=300
QUERY_ITERS=30
QUERY_CONCURRENCY=1,4
SEED=1

# note writes to the step summary (when configured) and to stdout.
note() {
  echo "bench-ci: $*"
  if [[ -n "$SUMMARY_FILE" ]]; then
    echo "$*" >>"$SUMMARY_FILE"
  fi
}

# run_logged LOGFILE CMD...: run CMD with its (verbose) output captured in
# LOGFILE, dumping the log on failure so CI shows what broke.
run_logged() {
  local log=$1
  shift
  if ! "$@" >"$log" 2>&1; then
    echo "bench-ci: FAILED: $*" >&2
    cat "$log" >&2
    return 1
  fi
}

# run_suite BIN ROOT: one full benchmark pass — cold ingest of the fixture
# chunk, a capped hot ingest, then cold and hot query sweeps against what the
# ingests produced. One CSV --out per command (they share file names).
run_suite() {
  local bin=$1 root=$2
  mkdir -p "$root"
  run_logged "$root/ingest-cold.log" \
    "$bin" bench-ingest cold --source=pack --pack-dir="$FIXTURE_DIR" \
    --chunk="$CHUNK" \
    --cold-out-dir="$root/cold-artifacts" --out="$root/ingest-cold"
  run_logged "$root/ingest-hot.log" \
    "$bin" bench-ingest hot --source=pack --pack-dir="$FIXTURE_DIR" \
    --chunk="$CHUNK" --num-ledgers="$HOT_LEDGERS" \
    --hot-dir="$root/hot-db" --out="$root/ingest-hot"
  run_logged "$root/query-cold.log" \
    "$bin" bench-query cold --types=ledgers,txpage,txhash,events \
    --cold-dir="$root/cold-artifacts" --chunk="$CHUNK" \
    --query-concurrency="$QUERY_CONCURRENCY" --iters="$QUERY_ITERS" \
    --sample-ledgers=10000 --page-size=5 --seed="$SEED" \
    --out="$root/query-cold"
  run_logged "$root/query-hot.log" \
    "$bin" bench-query hot --types=ledgers,txpage,txhash,events \
    --hot-dir="$root/hot-db" --chunk="$CHUNK" \
    --query-concurrency="$QUERY_CONCURRENCY" --iters="$QUERY_ITERS" \
    --warmup=3 --sample-ledgers="$HOT_LEDGERS" --page-size=2 \
    --ledgers-per-read=5 --seed="$SEED" --out="$root/query-hot"
}

# A baseline that predates the bench subcommands cannot be compared against;
# fall back to candidate-only data collection instead of failing every PR
# whose base branch lacks them.
if [[ "$SKIP_COMPARE" != 1 ]]; then
  if ! "$BASELINE_BIN" bench-ingest --help >/dev/null 2>&1 ||
    ! "$BASELINE_BIN" bench-query --help >/dev/null 2>&1; then
    note "baseline build lacks the bench subcommands; running candidate only, no comparison"
    SKIP_COMPARE=1
  fi
fi

echo "bench-ci: generating fixture chunk $CHUNK (seed $SEED) under $WORKDIR"
FIXTURE_DIR="$WORKDIR/fixture/ledgers"
run_logged "$WORKDIR/fixture.log" \
  "$CANDIDATE_BIN" bench-ingest fixture --pack-dir="$FIXTURE_DIR" \
  --chunk="$CHUNK" --seed="$SEED"

for run in $(seq 1 "$RUNS"); do
  # Alternate which side goes first so warm-up effects within a run pair
  # don't systematically favor one side.
  sides=(baseline candidate)
  if ((run % 2 == 0)); then
    sides=(candidate baseline)
  fi
  if [[ "$SKIP_COMPARE" == 1 ]]; then
    sides=(candidate)
  fi
  for side in "${sides[@]}"; do
    bin="$CANDIDATE_BIN"
    if [[ "$side" == baseline ]]; then
      bin="$BASELINE_BIN"
    fi
    echo "bench-ci: run $run/$RUNS $side"
    run_suite "$bin" "$WORKDIR/$side/run$run"
  done
done

if [[ "$SKIP_COMPARE" == 1 ]]; then
  note "comparison skipped; candidate CSVs are under $WORKDIR/candidate"
  exit 0
fi

compare_args=(--metrics-file "$METRICS_FILE")
for run in $(seq 1 "$RUNS"); do
  compare_args+=(--baseline-root "$WORKDIR/baseline/run$run")
  compare_args+=(--candidate-root "$WORKDIR/candidate/run$run")
done
if [[ -n "$SUMMARY_FILE" ]]; then
  compare_args+=(--summary "$SUMMARY_FILE")
fi
"$CANDIDATE_BIN" bench-compare "${compare_args[@]}"
