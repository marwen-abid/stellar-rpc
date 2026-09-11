#!/usr/bin/env bash
# Renders the Slack webhook payload (Block Kit) for the bench-campaign and
# bench-reaper notifications. The layouts are the sibling slack-*.jq files, so
# every variant renders on a laptop:
#   MODE=campaign STATE=ok CAMPAIGN_NAME=phase3-c6id8xl ./slack-payload.sh | jq .
#
# Env in, MODE=campaign: STATE (ok|fail), REASON, CAMPAIGN_NAME, PHASE, INGEST,
#   QUERY, RUNS, WORKERS, INSTANCE_TYPE, HOT_NUM_LEDGERS, BUDGET_MINUTES,
#   DEADLINE_EPOCH, TARGET_REF, BENCH_RUN_ID, VIEWER_URL, RESULTS_URI, RUN_URL,
#   RUN_JSON (path to the converted results-site run JSON), INGEST_STATE
#   (ingested|skipped|failed, empty when the ingest was never attempted),
#   INGEST_REASON (one line), EXCERPT (verdict-markdown lines, fail), BOX_ID,
#   BOX_RESCUED (true when the box is left up), RUN_ID_TAG, BUCKET, RESULT_KEY,
#   AWS_REGION, HARNESS_SHA, REPO_URL. ELAPSED_MINUTES and TARGET_SHA are
#   derived when unset or empty; NOW_EPOCH overrides the clock for tests.
# Env in, MODE=reaper: REAPED_JSON ('[{"id":"i-..","runId":"123..","overdueMin":42}, ...]'),
#   UNTAGGED (space-separated instance ids), RUN_URL, REPO_URL.
# Out: {attachments: [{color, fallback, blocks}]}. The one-line summary rides in
# the fallback (a top-level text field would render above the card). Slack folds
# a tall attachment behind "Show more", so blocks are ordered important-first.
# Also runs on developer macOS, i.e. bash 3.2 + BSD date.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)

MODE="${MODE:-campaign}"
REPO_URL="${REPO_URL:-https://github.com/stellar/stellar-rpc}"
AWS_REGION="${AWS_REGION:-us-east-1}"

# 252 -> "4 h 12 m"; 420 -> "7 h"; 45 -> "45 m"; junk -> "".
fmt_minutes() {
  case "${1:-}" in '' | *[!0-9]*) echo ""; return ;; esac
  if [ "$1" -ge 60 ] && [ "$(($1 % 60))" -eq 0 ]; then
    echo "$(($1 / 60)) h"
  elif [ "$1" -ge 60 ]; then
    echo "$(($1 / 60)) h $(($1 % 60)) m"
  else
    echo "$1 m"
  fi
}

# GNU date wants -d @epoch, BSD date wants -r epoch.
fmt_epoch_hm() {
  case "${1:-}" in '' | *[!0-9]*) echo ""; return ;; esac
  date -u -r "$1" +%H:%M 2>/dev/null || date -u -d "@$1" +%H:%M 2>/dev/null || echo ""
}

# S3 console https links (Slack cannot open a raw s3:// URI).
s3_object_url() {
  [ -n "$1" ] && [ -n "$2" ] || { echo ""; return; }
  echo "https://${AWS_REGION}.console.aws.amazon.com/s3/object/$1?region=${AWS_REGION}&prefix=$2"
}

s3_prefix_url() {
  case "${1:-}" in s3://*) ;; *) echo ""; return ;; esac
  local rest="${1#s3://}"
  local bucket="${rest%%/*}"
  local prefix="${rest#*/}"
  [ "$prefix" != "$rest" ] || prefix=""
  echo "https://${AWS_REGION}.console.aws.amazon.com/s3/buckets/${bucket}?region=${AWS_REGION}&prefix=${prefix}/"
}

if [ "$MODE" = "reaper" ]; then
  REAPED_JSON="${REAPED_JSON:-[]}"
  UNTAGGED="${UNTAGGED:-}"
  jq -n \
    --argjson reaped "$REAPED_JSON" \
    --arg untagged "$UNTAGGED" \
    --arg run_url "${RUN_URL:-}" \
    --arg repo "$REPO_URL" \
    -f "$DIR/slack-reaper.jq"
  exit 0
fi

[ "$MODE" = "campaign" ] || { echo "MODE must be campaign or reaper, got '$MODE'" >&2; exit 1; }
STATE="${STATE:-}"
[ "$STATE" = "ok" ] || [ "$STATE" = "fail" ] || { echo "STATE must be ok or fail, got '$STATE'" >&2; exit 1; }

CAMPAIGN_NAME="${CAMPAIGN_NAME:-<unnamed>}"
REASON="${REASON:-}"
PHASE="${PHASE:-}"
INGEST="${INGEST:-}"
QUERY="${QUERY:-}"
RUNS="${RUNS:-}"
WORKERS="${WORKERS:-}"
INSTANCE_TYPE="${INSTANCE_TYPE:-}"
HOT_NUM_LEDGERS="${HOT_NUM_LEDGERS:-0}"
TARGET_REF="${TARGET_REF:-}"
TARGET_SHA="${TARGET_SHA:-}"
BENCH_RUN_ID="${BENCH_RUN_ID:-}"
VIEWER_URL="${VIEWER_URL:-}"
RESULTS_URI="${RESULTS_URI:-}"
RUN_URL="${RUN_URL:-}"
RUN_JSON="${RUN_JSON:-}"
INGEST_STATE="${INGEST_STATE:-}"
INGEST_REASON="${INGEST_REASON:-}"
EXCERPT="${EXCERPT:-}"
BOX_ID="${BOX_ID:-}"
BOX_RESCUED="${BOX_RESCUED:-false}"
RUN_ID_TAG="${RUN_ID_TAG:-}"
BUCKET="${BUCKET:-}"
RESULT_KEY="${RESULT_KEY:-}"
HARNESS_SHA="${HARNESS_SHA:-}"
ELAPSED_MINUTES="${ELAPSED_MINUTES:-}"

# The render script pins deadline_epoch = render time + budget, so
# elapsed = now - (deadline - budget).
if [ -z "$ELAPSED_MINUTES" ] && [ -n "${DEADLINE_EPOCH:-}" ] && [ -n "${BUDGET_MINUTES:-}" ]; then
  case "$DEADLINE_EPOCH$BUDGET_MINUTES" in
    *[!0-9]*) : ;;
    *)
      NOW=${NOW_EPOCH:-$(date +%s)}
      E=$(( (NOW - (DEADLINE_EPOCH - BUDGET_MINUTES * 60)) / 60 ))
      if [ "$E" -ge 0 ]; then ELAPSED_MINUTES=$E; fi
      ;;
  esac
fi
# The benchmarked commit comes from the run JSON: campaign boxes leave the
# result object's targetSha empty.
if [ -z "$TARGET_SHA" ] && [ -n "$RUN_JSON" ] && [ -r "$RUN_JSON" ]; then
  TARGET_SHA=$(jq -r '.build.commit // ""' "$RUN_JSON" 2>/dev/null || true)
fi

ELAPSED_H=$(fmt_minutes "$ELAPSED_MINUTES")
BUDGET_H=$(fmt_minutes "${BUDGET_MINUTES:-}")
DEADLINE_HM=$(fmt_epoch_hm "${DEADLINE_EPOCH:-}")

BOXLOG_URL=""
VERDICT_URL=""
if [ -n "$BOX_ID" ] && [ -n "$BUCKET" ] && [ -n "$RESULT_KEY" ]; then
  BOXLOG_URL=$(s3_object_url "$BUCKET" "${RESULT_KEY%/*}/user-data.log")
  VERDICT_URL=$(s3_object_url "$BUCKET" "$RESULT_KEY")
fi
BUNDLE_URL=$(s3_prefix_url "$RESULTS_URI")

# The ingest emits the detail-viewer URL; the card links the summary page.
SUMMARY_URL=""
if [ -n "$VIEWER_URL" ]; then
  BASE="${VIEWER_URL%%\?*}"
  URL_QUERY="${VIEWER_URL#"$BASE"}"
  SUMMARY_URL="${BASE%/}/summary.html${URL_QUERY}"
fi

# Ingestion-p99-vs-target recap (slack-recap.jq); null drops the block.
RESULTS_BLOCK=null
if [ "$STATE" = "ok" ] && [ -n "$RUN_JSON" ] && [ -r "$RUN_JSON" ]; then
  RESULTS_BLOCK=$(jq -c -f "$DIR/slack-recap.jq" "$RUN_JSON" 2>/dev/null) || RESULTS_BLOCK=null
  [ -n "$RESULTS_BLOCK" ] || RESULTS_BLOCK=null
fi

# The one-line summary: notification preview and what post-slack.sh echoes.
if [ "$STATE" = "ok" ]; then
  TEXT="✅ bench campaign $CAMPAIGN_NAME passed — run_id=${BENCH_RUN_ID:-unknown} · ${SUMMARY_URL:-${RESULTS_URI:-s3://$BUCKET/$RESULT_KEY}} · $RUN_URL"
else
  TEXT="❌ bench campaign $CAMPAIGN_NAME failed: $REASON · $RUN_URL"
  if [ -n "$BOXLOG_URL" ]; then TEXT="$TEXT · box log (if uploaded): $BOXLOG_URL"; fi
fi

jq -n \
  --arg state "$STATE" \
  --arg text "$TEXT" \
  --arg name "$CAMPAIGN_NAME" \
  --arg reason "$REASON" \
  --arg phase "$PHASE" \
  --arg ingest "$INGEST" \
  --arg query "$QUERY" \
  --arg runs "$RUNS" \
  --arg workers "$WORKERS" \
  --arg machine "$INSTANCE_TYPE" \
  --arg hot_cap "$HOT_NUM_LEDGERS" \
  --arg elapsed "$ELAPSED_H" \
  --arg budget "$BUDGET_H" \
  --arg deadline_hm "$DEADLINE_HM" \
  --arg ref "$TARGET_REF" \
  --arg sha "$TARGET_SHA" \
  --arg bench "$BENCH_RUN_ID" \
  --arg summary "$SUMMARY_URL" \
  --arg run_url "$RUN_URL" \
  --arg bundle_url "$BUNDLE_URL" \
  --arg boxlog_url "$BOXLOG_URL" \
  --arg verdict_url "$VERDICT_URL" \
  --arg excerpt "$EXCERPT" \
  --arg box "$BOX_ID" \
  --arg rescued "$BOX_RESCUED" \
  --arg runidtag "$RUN_ID_TAG" \
  --arg repo "$REPO_URL" \
  --arg harness "$HARNESS_SHA" \
  --arg ingest_state "$INGEST_STATE" \
  --arg ingest_reason "$INGEST_REASON" \
  --argjson results "$RESULTS_BLOCK" \
  -f "$DIR/slack-campaign.jq"
