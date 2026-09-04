#!/usr/bin/env bash
# Renders the EC2 user-data for a bench-campaign box.
#
# The scripts ship verbatim; parameters travel in a preamble of exports, so the
# bytes that run on the box match the bytes in git. Order is:
#   preamble exports -> bootstrap-common.sh -> run-campaign.sh
# BENCH_TOML_B64 is one base64 line, so it survives quoting as-is.
#
# The result is gzipped: cloud-init gunzips user-data transparently, and the raw
# script outgrew EC2's 16 KB user-data cap. The 16 KB cap is checked on the
# gzipped size before launch.
#
# env in:  RUN_ID BUCKET RESULT_KEY SELF_TERMINATE_MINUTES BUDGET_MINUTES
#          BENCH_TOML_B64 BENCH_REPO_REF
# out:     $OUT (raw script, default /tmp/user-data.sh) and $OUT.gz
set -euo pipefail
DIR=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-/tmp/user-data.sh}

{
  echo '#!/usr/bin/env bash'
  echo "export RUN_ID=$RUN_ID"
  echo "export BUCKET=$BUCKET RESULT_KEY=$RESULT_KEY"
  echo "export SELF_TERMINATE_MINUTES=$SELF_TERMINATE_MINUTES BUDGET_MINUTES=$BUDGET_MINUTES"
  echo "export BENCH_TOML_B64=\"$BENCH_TOML_B64\""
  echo "export BENCH_REPO_REF=\"$BENCH_REPO_REF\""
  cat "$DIR/../bootstrap-common.sh"
  cat "$DIR/run-campaign.sh"
} > "$OUT"
gzip -9 -c "$OUT" > "$OUT.gz"
RAW=$(wc -c < "$OUT")
SIZE=$(wc -c < "$OUT.gz")
echo "user-data is $RAW bytes raw, $SIZE bytes gzipped"
[ "$SIZE" -le 16384 ] || { echo "::error::gzipped user-data is $SIZE bytes, over the 16384-byte EC2 limit"; exit 1; }
