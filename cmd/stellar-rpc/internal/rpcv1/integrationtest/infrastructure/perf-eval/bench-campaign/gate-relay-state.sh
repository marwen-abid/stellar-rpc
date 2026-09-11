#!/usr/bin/env bash
# Turns the relay's state output into the poll job's verdict. The relay exits 0
# for every state; this gate decides.
# Env: STATE=ok|fail|running; NEXT is the next window's label. NEXT unset means
# the last window: `running` fails because the campaign produced no verdict.
set -euo pipefail

case "${STATE:-}" in
  ok) echo "campaign passed inside this window" ;;
  running)
    if [ -n "${NEXT:-}" ]; then
      echo "window closed with the campaign still running; handing off to $NEXT"
    else
      echo "::error::relay chain exhausted; the campaign is still running on the box"
      exit 1
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
