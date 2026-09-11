#!/usr/bin/env bash
# Reads this attempt's notification context from S3, keyed on the verdict.
# Env: STATE (ok|fail), RUN_ID (run_id-run_attempt), BUCKET, RESULT_KEY.
# Outputs to GITHUB_OUTPUT (stdout when unset):
#   ok:   bench_run_id, results_uri, tarball_key from the run-info.json sidecar
#         the box writes next to the result object (no markdown parsing).
#   fail: excerpt, a few lines of the result object's verdict markdown.
# Both check the object's runId against RUN_ID the way the relay does.
# Best-effort: no output means fall back to the workflow diagnostics.
set -euo pipefail
WORK=$(mktemp -d "${TMPDIR:-/tmp}/campaign-context-XXXXXX")
trap 'rm -rf "$WORK"' EXIT

case "$STATE" in
  ok)
    if aws s3 cp "s3://$BUCKET/${RESULT_KEY%/*}/run-info.json" "$WORK/run-info.json" >/dev/null 2>&1; then
      if jq -e --arg run "$RUN_ID" '
        .schemaVersion == 1 and .runId == $run and
        ([.benchRunId, .resultsUri, .tarballKey, (.benchmarksSha // "")]
          | all(type == "string" and (test("[\\r\\n]") | not)))
        ' "$WORK/run-info.json" >/dev/null 2>&1; then
        jq -r '"bench_run_id=\(.benchRunId)", "results_uri=\(.resultsUri)",
          "tarball_key=\(.tarballKey)", "benchmarks_sha=\(.benchmarksSha // "")"' \
          "$WORK/run-info.json" >> "${GITHUB_OUTPUT:-/dev/stdout}"
      else
        echo "::warning::invalid or stale run-info.json; the notification falls back to workflow diagnostics"
      fi
    else
      echo "::warning::no run-info.json sidecar; the notification falls back to the result key"
    fi
    ;;
  fail)
    # The markdown holds the box's last console lines, the closest thing to a
    # cause. The seeded pending marker has no markdown and yields nothing.
    aws s3 cp "s3://$BUCKET/$RESULT_KEY" "$WORK/result.json" >/dev/null 2>&1 || exit 0
    jq -e --arg run "$RUN_ID" '.schemaVersion == 1 and .runId == $run and
      .verdict == "fail" and (.markdown | type == "string")' \
      "$WORK/result.json" >/dev/null 2>&1 || exit 0
    # Drop fences, blank lines and the ✅/❌ headline (the Slack header carries
    # the verdict); keep the first grep-worthy lines, else the last three.
    jq -r '.markdown // ""' "$WORK/result.json" | grep -v '^```' | grep -v '^\s*$' | grep -v '^[✅❌]' > "$WORK/verdict-lines.txt" || true
    EXCERPT=$(grep -m3 -iE 'fail|error|regression|gate|exceed|panic' "$WORK/verdict-lines.txt" || tail -n 3 "$WORK/verdict-lines.txt")
    [ -n "$EXCERPT" ] || exit 0
    DELIMITER=VERDICT_EXCERPT_EOF
    while printf '%s\n' "$EXCERPT" | grep -qxF "$DELIMITER"; do DELIMITER="${DELIMITER}_"; done
    {
      echo "excerpt<<$DELIMITER"
      echo "$EXCERPT"
      echo "$DELIMITER"
    } >> "${GITHUB_OUTPUT:-/dev/stdout}"
    ;;
esac
