#!/usr/bin/env bash
# Renders the stellar-rpc-benchmarks campaign config from the bench-campaign.yml
# dispatch inputs, plus the launch parameters derived from it. Runs on a laptop:
#
#   PHASE=1 INGEST=both QUERY=no MACHINE=2x ./render-campaign-toml.sh
#
# Env in: PHASE INGEST QUERY RUNS WORKERS MACHINE TARGET_REF HOT_NUM_LEDGERS
# RUN_NAME BENCHMARKS_REF; PUBLISH_URI INPUTS_PREFIX CAPACITY_MINUTES
# SETUP_MARGIN_MINUTES QUERY_LEG_DURATION_SECONDS NOW_EPOCH are test overrides.
# BENCHMARKS_REF has no output key: the launch job reads the dispatch input.
# Out: key=value lines appended to $GITHUB_OUTPUT (stdout when unset); the
# human-readable estimate goes to stderr.
# Must run on bash 3.2 (macOS): no associative arrays, no `base64 -w0`.
set -euo pipefail

die() {
  echo "::error::$*" >&2
  exit 1
}

is_uint() {
  case "${1:-}" in
    '' | *[!0-9]*) return 1 ;;
    *) return 0 ;;
  esac
}

# require_uint <name> <value> <min> <max>
require_uint() {
  is_uint "$2" || die "$1 must be an integer, got '$2'"
  if [ "$2" -lt "$3" ] || [ "$2" -gt "$4" ]; then
    die "$1 must be between $3 and $4, got '$2'"
  fi
}

# GNU date wants -d @epoch, BSD date wants -r epoch.
fmt_epoch() {
  date -u -r "$1" +%FT%TZ 2>/dev/null || date -u -d "@$1" +%FT%TZ 2>/dev/null || echo "epoch $1"
}

PHASE="${PHASE:-}"
INGEST="${INGEST:-both}"
QUERY="${QUERY:-no}"
RUNS="${RUNS:-1}"
WORKERS="${WORKERS:-}"
MACHINE="${MACHINE:-2x}"
TARGET_REF="${TARGET_REF:-feature/full-history}"
HOT_NUM_LEDGERS="${HOT_NUM_LEDGERS:-0}"
RUN_NAME="${RUN_NAME:-}"
BENCHMARKS_REF="${BENCHMARKS_REF:-main}"

PUBLISH_URI="${PUBLISH_URI:-s3://stellar-rpc-bench/results}"
INPUTS_PREFIX="${INPUTS_PREFIX:-s3://stellar-rpc-bench/inputs/synthetic-ledgers/2026-07-18-apply-load-20k}"
# 21 h = four 5.3 h poller windows, the most the relay chain can cover.
CAPACITY_MINUTES="${CAPACITY_MINUTES:-1260}"
# Toolchain install, build, dataset sync, cold ingest legs, bundle upload.
SETUP_MARGIN_MINUTES="${SETUP_MARGIN_MINUTES:-120}"
NOW_EPOCH="${NOW_EPOCH:-$(date +%s)}"

# How long a bench-query leg holds each ladder rate. Mirrors the runner's
# DefaultQueryDuration (runner/internal/config): the TOML cannot set it because
# the runner rejects unknown keys.
QUERY_LEG_DURATION_SECONDS="${QUERY_LEG_DURATION_SECONDS:-60}"
# The runner's query grid (runner/internal/plan/plan.go): one leg per dataset,
# run, tier (cold, hot) and type (ledgers, txpage, txhash, events).
query_tiers=2
query_type_count=4
# Three ladder rates (benchmarks docs/targets.json: 0.5x, 1x, 2x of the phase
# floor), QUERY_LEG_DURATION_SECONDS each.
query_rate_steps=3
# Per-leg time outside the paced windows: corpus build, cold-leg cache drop, warmup.
query_leg_overhead_secs=60

case "$INGEST" in
  both | cold | hot | none) ;;
  *) die "ingest must be one of both|cold|hot|none, got '$INGEST'" ;;
esac
case "$QUERY" in
  yes | no) ;;
  *) die "query must be yes|no, got '$QUERY'" ;;
esac
case "$MACHINE" in
  2x) instance_type=m6id.2xlarge; default_workers=8 ;;
  8x) instance_type=c6id.8xlarge; default_workers=32 ;;
  *) die "machine must be 2x|8x, got '$MACHINE'" ;;
esac

# Phase = the pacing each dataset was generated for; profiles = S3 dirs under INPUTS_PREFIX.
case "$PHASE" in
  1) close_interval=2s;    interval_ms=2000; profiles="sac-6000 custom_token-4000 soroswap-1500" ;;
  2) close_interval=1s;    interval_ms=1000; profiles="sac-5000 custom_token-4000 soroswap-1500" ;;
  3) close_interval=600ms; interval_ms=600;  profiles="sac-6000 custom_token-3600 soroswap-1800" ;;
  *) die "phase must be 1|2|3, got '$PHASE'" ;;
esac

require_uint runs "$RUNS" 1 20
require_uint query_leg_duration_seconds "$QUERY_LEG_DURATION_SECONDS" 1 3600
# A campaign runs chunk 1 only, so a cap above the 10,000-ledger chunk is a typo.
require_uint hot_num_ledgers "$HOT_NUM_LEDGERS" 0 10000
if [ -n "$WORKERS" ]; then
  require_uint workers "$WORKERS" 1 128
  workers="$WORKERS"
else
  workers="$default_workers"
fi

if [ "$INGEST" = none ] && [ "$QUERY" = no ]; then
  die "ingest=none with query=no is an empty campaign; pick an ingest mode or set query=yes"
fi
# Query legs read databases this campaign builds: hot legs the hot ingest's DB,
# cold legs the cold ingest's store (the banked golden cold tree predates the #932
# cold-index format). Without both ingests a query leg fails an hour into a paid run.
if [ "$QUERY" = yes ] && [ "$INGEST" != both ]; then
  die "query=yes needs both databases: use ingest=both (cold legs read the cold ingest's store, hot legs the hot ingest's DB), got ingest=$INGEST"
fi

# These values reach commands on the box as arguments (`git checkout "$ref"`,
# the runner CLI, the instance Name tag), where a leading `-` reads as an option.
campaign_name="${RUN_NAME:-phase${PHASE}-${MACHINE}}"
case "$campaign_name" in
  -*) die "run_name must not start with '-' (it is read as a command option), got '$campaign_name'" ;;
  *[!A-Za-z0-9._-]*) die "run_name must match [A-Za-z0-9._-]+ (the runner rejects anything else), got '$campaign_name'" ;;
esac
# Interpolated unescaped into the TOML `ref`, so git-ref characters only.
case "$TARGET_REF" in
  -*) die "ref must not start with '-' (it is read as a command option), got '$TARGET_REF'" ;;
  *[!A-Za-z0-9._/-]*) die "ref must match [A-Za-z0-9._/-]+, got '$TARGET_REF'" ;;
esac
# Interpolated into the box user-data, which runs as root.
case "$BENCHMARKS_REF" in
  -*) die "benchmarks_ref must not start with '-' (it is read as a command option), got '$BENCHMARKS_REF'" ;;
  *[!A-Za-z0-9._/-]*) die "benchmarks_ref must match [A-Za-z0-9._/-]+, got '$BENCHMARKS_REF'" ;;
esac

# Paced parts have a known wall clock: hot ingest legs replay `ledgers` ledgers at
# the phase close interval, one per dataset per run; query legs hold each ladder
# rate for a fixed duration. Cold ingest is unpaced and sits in the setup margin.
dataset_count=0
for _p in $profiles; do
  dataset_count=$((dataset_count + 1))
done

if [ "$INGEST" = hot ] || [ "$INGEST" = both ]; then
  if [ "$HOT_NUM_LEDGERS" -gt 0 ]; then
    ledgers="$HOT_NUM_LEDGERS"
  else
    ledgers=10000
  fi
  hot_secs=$(((dataset_count * RUNS * ledgers * interval_ms + 999) / 1000))
else
  hot_secs=0
fi

if [ "$QUERY" = yes ]; then
  query_legs=$((dataset_count * RUNS * query_tiers * query_type_count))
  query_secs=$((query_legs * (query_rate_steps * QUERY_LEG_DURATION_SECONDS + query_leg_overhead_secs)))
else
  query_legs=0
  query_secs=0
fi
budget_minutes=$(((hot_secs + query_secs + 59) / 60 + SETUP_MARGIN_MINUTES))

if [ "$budget_minutes" -gt "$CAPACITY_MINUTES" ]; then
  hint="shrink runs (${RUNS}), cap hot_num_ledgers (${HOT_NUM_LEDGERS}), or split the phase into separate dispatches"
  if [ "$QUERY" = yes ]; then
    hint="$hint; the ${query_legs} query legs (${query_rate_steps} rates of ${QUERY_LEG_DURATION_SECONDS}s each) cost ~$((query_secs / 60))m of it, so query=no is the other lever"
  fi
  die "estimated budget ${budget_minutes}m exceeds the ${CAPACITY_MINUTES}m relay-chain ceiling;" "$hint"
fi

deadline_epoch=$((NOW_EPOCH + budget_minutes * 60))
# The box outlives the relay chain so a stuck run still uploads its log.
self_terminate_minutes=$((budget_minutes + 30))

query_bool=false
[ "$QUERY" = yes ] && query_bool=true

toml_file="$(mktemp "${TMPDIR:-/tmp}/campaign-XXXXXX")"
{
  printf 'name = "%s"\n' "$campaign_name"
  printf 'ref = "%s"\n' "$TARGET_REF"
  printf 'ingest = "%s"\n' "$INGEST"
  printf 'query = %s\n' "$query_bool"
  printf 'close_interval = "%s"\n' "$close_interval"
  printf 'runs = %s\n' "$RUNS"
  printf 'workers = %s\n' "$workers"
  printf 'hot_num_ledgers = %s\n' "$HOT_NUM_LEDGERS"
  printf 'publish_uri = "%s"\n' "$PUBLISH_URI"
  for profile in $profiles; do
    printf '\n[[dataset]]\n'
    printf 'name = "%s"\n' "$profile"
    printf 'kind = "packs-s3"\n'
    # packs-v2: #910 added an app-data CRC the reader verifies; packs/cold is pre-#910.
    printf 'location = "%s/%s/packs-v2/cold"\n' "$INPUTS_PREFIX" "$profile"
    printf 'chunks = [1]\n'
  done
} > "$toml_file"

{
  echo "toml_b64=$(base64 < "$toml_file" | tr -d '\n')"
  echo "budget_minutes=$budget_minutes"
  echo "deadline_epoch=$deadline_epoch"
  echo "instance_type=$instance_type"
  echo "workers=$workers"
  echo "self_terminate_minutes=$self_terminate_minutes"
  echo "campaign_name=$campaign_name"
} >> "${GITHUB_OUTPUT:-/dev/stdout}"

{
  echo "campaign $campaign_name: phase $PHASE (close_interval $close_interval), ingest=$INGEST query=$QUERY"
  echo "datasets: $profiles"
  echo "machine $MACHINE = $instance_type, workers $workers, runs $RUNS, hot_num_ledgers $HOT_NUM_LEDGERS"
  echo "paced hot legs ~$((hot_secs / 60))m; ${query_legs} query legs ~$((query_secs / 60))m; budget ${budget_minutes}m of ${CAPACITY_MINUTES}m capacity"
  echo "deadline $(fmt_epoch "$deadline_epoch"); box self-terminates after ${self_terminate_minutes}m"
  echo "rendered config: $toml_file"
} >&2
