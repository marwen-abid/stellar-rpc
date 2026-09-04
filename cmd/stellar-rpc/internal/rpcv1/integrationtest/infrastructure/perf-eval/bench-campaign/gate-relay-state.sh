#!/usr/bin/env bash
# Turns the relay's state output into the poll job's verdict. The relay exits 0
# for every state; this gate decides.
# Env: STATE=ok|fail|running; NEXT is the next window's label. NEXT unset means
# the last window: `running` there only warns, because the box's self-terminate
# ceiling and the reaper still end the campaign.
set -euo pipefail

case "${STATE:-}" in
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
