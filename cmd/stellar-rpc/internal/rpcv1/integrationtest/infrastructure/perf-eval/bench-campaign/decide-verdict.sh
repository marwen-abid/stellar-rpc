#!/usr/bin/env bash
# Turns the poll chain's outputs into the campaign verdict. The chain's shape
# carries it: exactly one poll job sees the result object, the rest are skipped
# (empty state) or handed off. `rescued` mirrors the cleanup job's box-left-up
# condition (a poll saw a fail verdict), so the notification only claims a
# rescuable box when cleanup actually left one running.
# Env: S1 S2 S3 S4 (poll window states), VALIDATE_RESULT, LAUNCH_RESULT.
# Output: state, reason, rescued, appended to GITHUB_OUTPUT (stdout when unset).
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
