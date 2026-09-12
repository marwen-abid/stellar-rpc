#!/usr/bin/env bash
# Require a fresh, readable attempt key before EC2 launch. Never seed a result.
# Env: BUCKET, RESULT_KEY. The workflow role needs GetObject and ListBucket so
# S3 reports an absent key as NoSuchKey rather than AccessDenied.
set -euo pipefail
WORK=$(mktemp -d "${TMPDIR:-/tmp}/campaign-key-XXXXXX")
trap 'rm -rf "$WORK"' EXIT

if aws s3api get-object --bucket "$BUCKET" --key "$RESULT_KEY" \
    --cli-connect-timeout 10 --cli-read-timeout 30 \
    "$WORK/result.json" > /dev/null 2> "$WORK/error"; then
  echo "::error::result key already exists; refusing to overwrite this attempt"
  exit 1
fi
if grep -qF '(NoSuchKey)' "$WORK/error"; then
  echo "result key is absent and accessible; relay can wait for the box"
  exit 0
fi
cat "$WORK/error" >&2
echo "::error::cannot verify result-key access; confirm s3:GetObject and s3:ListBucket permissions" >&2
exit 1
