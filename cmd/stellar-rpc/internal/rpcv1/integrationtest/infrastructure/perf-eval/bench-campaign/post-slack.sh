#!/usr/bin/env bash
# Renders the Slack payload via the sibling slack-payload.sh and posts it to
# WEBHOOK; used by bench-campaign.yml and bench-reaper.yml. Degrade, never fail:
# a payload bug or a missing webhook must not turn a verdict red or fail a reap.
# Env in: WEBHOOK plus every slack-payload.sh input for the MODE. The one-line
# summary is echoed and appended to GITHUB_STEP_SUMMARY when that is set.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
PAYLOAD=$(mktemp "${TMPDIR:-/tmp}/slack-XXXXXX")
trap 'rm -f "$PAYLOAD"' EXIT

if ! "$DIR/slack-payload.sh" > "$PAYLOAD"; then
  echo "::warning::slack-payload.sh failed; notification not delivered"
  exit 0
fi
TEXT=$(jq -r '.attachments[0].fallback' "$PAYLOAD")
echo "$TEXT"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "$TEXT" >> "$GITHUB_STEP_SUMMARY"
if [ -z "${WEBHOOK:-}" ]; then
  echo "::warning::SLACK_BENCH_WEBHOOK_URL is unset; notification not delivered"
  exit 0
fi
if curl -fsS --connect-timeout 10 --max-time 30 -X POST -H 'Content-Type: application/json' \
     --data @"$PAYLOAD" "$WEBHOOK" >/dev/null; then
  echo "slack notified"
else
  echo "::warning::slack notification failed"
fi
