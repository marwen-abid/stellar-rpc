#!/usr/bin/env bash
# Assembles box user-data as a preamble + bootstrap-common.sh + run-campaign.sh.
# The scripts stay byte-identical to git; EC2 limits user-data to 16 KB.
# Env in: RUN_ID, BUCKET, RESULT_KEY, SELF_TERMINATE_MINUTES, BUDGET_MINUTES,
# BENCH_TOML_B64, BENCH_REPO_REF. Output: ${OUT:-/tmp/user-data.sh}.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-/tmp/user-data.sh}

{
  echo '#!/usr/bin/env bash'
  echo "export RUN_ID=$RUN_ID"
  echo "export BUCKET=$BUCKET RESULT_KEY=$RESULT_KEY"
  echo "export SELF_TERMINATE_MINUTES=$SELF_TERMINATE_MINUTES BUDGET_MINUTES=$BUDGET_MINUTES"
  # BENCH_TOML_B64 is one base64 line (the render script strips newlines), so
  # it survives this quoting.
  echo "export BENCH_TOML_B64=\"$BENCH_TOML_B64\""
  echo "export BENCH_REPO_REF=\"$BENCH_REPO_REF\""
  cat "$DIR/../bootstrap-common.sh"
  cat "$DIR/run-campaign.sh"
} > "$OUT"
SIZE=$(wc -c < "$OUT")
echo "user-data is $SIZE bytes"
[ "$SIZE" -le 16384 ] || { echo "::error::user-data is $SIZE bytes, over the 16384-byte EC2 limit"; exit 1; }
