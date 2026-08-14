#!/usr/bin/env bash
# Reads best-effort notification context from S3 for one workflow attempt.
# Env: STATE, RUN_ID, BUCKET, RESULT_KEY.
# STATE=ok outputs bench_run_id, results_uri, tarball_key; fail may output excerpt.
# Both paths may emit no output, letting the notification use the result key.
# Local: STATE=ok RUN_ID=123-1 BUCKET=bucket RESULT_KEY=runs/123/campaign/result.json ./fetch-result-context.sh
set -euo pipefail

case "$STATE" in
  ok)
    if aws s3 cp "s3://$BUCKET/${RESULT_KEY%/*}/run-info.json" /tmp/run-info.json >/dev/null 2>&1; then
      SIDECAR_RUN_ID=$(jq -r '.runId // ""' /tmp/run-info.json)
      # Attempts share this key, so the sidecar must belong to this attempt.
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
    aws s3 cp "s3://$BUCKET/$RESULT_KEY" /tmp/result.json >/dev/null 2>&1 || exit 0
    [ "$(jq -r '.runId // ""' /tmp/result.json)" = "$RUN_ID" ] || exit 0
    [ "$(jq -r '.verdict // ""' /tmp/result.json)" = "fail" ] || exit 0
    # Drop fences, blanks, and the verdict emoji; the Slack header has the verdict.
    # The seeded pending marker has no markdown, so it yields no excerpt.
    jq -r '.markdown // ""' /tmp/result.json | grep -v '^```' | grep -v '^\s*$' | grep -v '^[✅❌]' > /tmp/verdict-lines.txt || true
    EXCERPT=$(grep -m3 -iE 'fail|error|regression|gate|exceed|panic' /tmp/verdict-lines.txt || tail -n 3 /tmp/verdict-lines.txt)
    [ -n "$EXCERPT" ] || exit 0
    {
      echo "excerpt<<VERDICT_EXCERPT_EOF"
      echo "$EXCERPT"
      echo "VERDICT_EXCERPT_EOF"
    } >> "${GITHUB_OUTPUT:-/dev/stdout}"
    ;;
  *)
    exit 0
    ;;
esac
