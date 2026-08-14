#!/usr/bin/env bash
# Maps poll window states and validate/launch results to state, reason, and rescued.
# Exactly one poll job sees the result object; the others skip or hand off.
# rescued matches the cleanup job's box-left-up condition.
# Local: S1=ok S2= S3= S4= VALIDATE_RESULT=success LAUNCH_RESULT=success ./decide-verdict.sh
set -euo pipefail

STATE=fail
REASON=""
RESCUED=false
for S in "$S1" "$S2" "$S3" "$S4"; do
  if [ "$S" = "ok" ]; then STATE=ok; fi
done
if [ "$STATE" != "ok" ]; then
  if [ "$VALIDATE_RESULT" != "success" ]; then
    REASON="input validation failed"
  elif [ "$LAUNCH_RESULT" != "success" ]; then
    REASON="box launch failed"
  elif [ "$S1" = "fail" ] || [ "$S2" = "fail" ] || [ "$S3" = "fail" ] || [ "$S4" = "fail" ]; then
    REASON="the campaign reported a failing verdict"
    RESCUED=true
  else
    REASON="the relay chain was exhausted without a verdict"
  fi
fi
{
  echo "state=$STATE"
  echo "reason=$REASON"
  echo "rescued=$RESCUED"
} >> "${GITHUB_OUTPUT:-/dev/stdout}"
echo "verdict: $STATE ${REASON:+($REASON)}"
