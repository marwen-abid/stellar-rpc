#!/usr/bin/env bash
set -euo pipefail
# Turns the relay's state output into the poll job's verdict.
# Env: STATE=ok|fail|running; NEXT is the next-window label. Unset NEXT means
# the last window: running only warns, and the box's own self-terminate
# ceiling plus the reaper still end the campaign.

case "$STATE" in
  ok) echo "campaign passed inside this window" ;;
  running)
    if [ -n "${NEXT:-}" ]; then
      echo "window closed with the campaign still running; handing off to $NEXT"
    else
      echo "::warning::relay chain exhausted; the campaign is still running on the box"
    fi
    ;;
  fail)
    cat /tmp/results.md /tmp/timeout-comment.md 2>/dev/null || true
    echo "::error::campaign failed"
    exit 1
    ;;
  *)
    echo "::error::relay reported no state"
    exit 1
    ;;
esac
