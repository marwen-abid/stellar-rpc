#!/usr/bin/env bash
# Reads this attempt's notification context from S3, keyed on the verdict.
# Env: STATE (ok|fail), RUN_ID (run_id-run_attempt), BUCKET, RESULT_KEY.
# Outputs to GITHUB_OUTPUT (stdout when unset):
#   ok:   bench_run_id, results_uri, tarball_key from the run-info.json sidecar
#         the box writes next to the result object (no markdown parsing).
#   fail: excerpt, a few lines of the result object's verdict markdown.
# Both check the object's runId against RUN_ID the way the relay does: attempts
# share the key, so a re-run whose own upload failed would otherwise report the
# previous attempt's. Best-effort: no output means fall back to the result key.
set -euo pipefail

case "$STATE" in
  ok)
    if aws s3 cp "s3://$BUCKET/${RESULT_KEY%/*}/run-info.json" /tmp/run-info.json >/dev/null 2>&1; then
      SIDECAR_RUN_ID=$(jq -r '.runId // ""' /tmp/run-info.json)
      if [ "$SIDECAR_RUN_ID" = "$RUN_ID" ]; then
        {
          echo "bench_run_id=$(jq -r '.benchRunId // ""' /tmp/run-info.json)"
          echo "results_uri=$(jq -r '.resultsUri // ""' /tmp/run-info.json)"
          echo "tarball_key=$(jq -r '.tarballKey // ""' /tmp/run-info.json)"
        } >> "${GITHUB_OUTPUT:-/dev/stdout}"
      else
        echo "::warning::run-info.json is from run ${SIDECAR_RUN_ID:-<none>}, not $RUN_ID; the notification falls back to the result key"
      fi
    else
      echo "::warning::no run-info.json sidecar; the notification falls back to the result key"
    fi
    ;;
  fail)
    # The markdown holds the box's last console lines, the closest thing to a
    # cause. The seeded pending marker has no markdown and yields nothing.
    aws s3 cp "s3://$BUCKET/$RESULT_KEY" /tmp/result.json >/dev/null 2>&1 || exit 0
    [ "$(jq -r '.runId // ""' /tmp/result.json)" = "$RUN_ID" ] || exit 0
    [ "$(jq -r '.verdict // ""' /tmp/result.json)" = "fail" ] || exit 0
    # Drop fences, blank lines and the ✅/❌ headline (the Slack header carries
    # the verdict); keep the first grep-worthy lines, else the last three.
    jq -r '.markdown // ""' /tmp/result.json | grep -v '^```' | grep -v '^\s*$' | grep -v '^[✅❌]' > /tmp/verdict-lines.txt || true
    EXCERPT=$(grep -m3 -iE 'fail|error|regression|gate|exceed|panic' /tmp/verdict-lines.txt || tail -n 3 /tmp/verdict-lines.txt)
    [ -n "$EXCERPT" ] || exit 0
    {
      echo "excerpt<<VERDICT_EXCERPT_EOF"
      echo "$EXCERPT"
      echo "VERDICT_EXCERPT_EOF"
    } >> "${GITHUB_OUTPUT:-/dev/stdout}"
    ;;
esac
