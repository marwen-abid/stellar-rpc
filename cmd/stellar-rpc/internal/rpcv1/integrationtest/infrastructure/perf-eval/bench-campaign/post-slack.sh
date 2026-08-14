#!/usr/bin/env bash
# Renders and posts a Slack payload without allowing notification failure to change the verdict.
# Env in: WEBHOOK and all slack-payload.sh inputs; output is status text and optional step summary.
# Local: MODE=campaign STATE=ok CAMPAIGN_NAME=test ./post-slack.sh
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
