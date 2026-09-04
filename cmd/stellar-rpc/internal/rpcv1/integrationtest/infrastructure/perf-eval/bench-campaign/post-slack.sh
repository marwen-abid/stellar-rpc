#!/usr/bin/env bash
# Renders the Slack payload via the sibling slack-payload.sh and posts it to
# WEBHOOK; used by bench-campaign.yml and bench-reaper.yml. Degrade, never fail:
# a payload bug or a missing webhook must not turn a verdict red or fail a reap.
# Env in: WEBHOOK plus every slack-payload.sh input for the MODE. The one-line
# summary is echoed and appended to GITHUB_STEP_SUMMARY when that is set.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)

if ! "$DIR/slack-payload.sh" > /tmp/slack.json; then
  echo "::warning::slack-payload.sh failed; notification not delivered"
  exit 0
fi
TEXT=$(jq -r '.attachments[0].fallback' /tmp/slack.json)
echo "$TEXT"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "$TEXT" >> "$GITHUB_STEP_SUMMARY"
if [ -z "${WEBHOOK:-}" ]; then
  echo "::warning::SLACK_BENCH_WEBHOOK_URL is unset; notification not delivered"
  exit 0
fi
if curl -fsS -X POST -H 'Content-Type: application/json' \
     --data @/tmp/slack.json "$WEBHOOK" >/dev/null; then
  echo "slack notified"
else
  echo "::warning::slack notification failed"
fi
