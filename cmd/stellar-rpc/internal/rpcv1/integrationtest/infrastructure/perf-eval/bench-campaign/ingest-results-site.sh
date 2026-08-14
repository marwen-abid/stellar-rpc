#!/usr/bin/env bash
# Best-effort ingestion of a campaign bundle into the results site.
# Env: TARBALL_KEY, RESULTS_URI, PUSH_TOKEN, BUCKET.
# Outputs: viewer_url and run_json when ingestion succeeds.
# Local: BUCKET=b TARBALL_KEY=k RESULTS_URI=s3://b/k PUSH_TOKEN=token ./ingest-results-site.sh
set -euo pipefail

if [ -z "$PUSH_TOKEN" ]; then
  echo "::warning::BENCHMARKS_PUSH_TOKEN is unset; run not ingested into the results site"
  exit 0
fi
if ! aws s3 cp "s3://$BUCKET/$TARBALL_KEY" /tmp/bench-bundle.tgz; then
  echo "::warning::tarball download failed; run not ingested into the results site"
  exit 0
fi
# Full clone: ingest.sh pushes HEAD:main, and a shallow clone's push can be
# rejected server-side. The token rides in the remote URL on an ephemeral
# runner, with Actions masking it in logs.
if ! git clone "https://x-access-token:${PUSH_TOKEN}@github.com/stellar-experimental/stellar-rpc-benchmarks.git" /tmp/benchmarks; then
  echo "::warning::benchmarks repo clone failed; run not ingested into the results site"
  exit 0
fi
git -C /tmp/benchmarks config user.name "github-actions[bot]"
git -C /tmp/benchmarks config user.email "41898282+github-actions[bot]@users.noreply.github.com"
# Campaign datasets are the synthetic apply-load packs, hence the hard-coded
# kind; revisit if a pubnet campaign ever runs through this workflow.
if (cd /tmp/benchmarks && scripts/ingest.sh /tmp/bench-bundle.tgz \
      --dataset-kind synthetic --push-main \
      -- --source-uri "$RESULTS_URI") 2>&1 | tee /tmp/ingest.log; then
  VIEWER=$(grep '^viewer: ' /tmp/ingest.log | tail -1 | cut -d' ' -f2)
  echo "viewer_url=$VIEWER" >> "${GITHUB_OUTPUT:-/dev/stdout}"
  SITE_RUN_ID="${VIEWER##*run=}"
  if [ -n "$SITE_RUN_ID" ] && [ -f "/tmp/benchmarks/docs/runs/$SITE_RUN_ID.json" ]; then
    echo "run_json=/tmp/benchmarks/docs/runs/$SITE_RUN_ID.json" >> "${GITHUB_OUTPUT:-/dev/stdout}"
  fi
  echo "ingested; viewer: $VIEWER"
else
  echo "::warning::results-site ingest failed; the notification falls back to the raw results URI"
fi
