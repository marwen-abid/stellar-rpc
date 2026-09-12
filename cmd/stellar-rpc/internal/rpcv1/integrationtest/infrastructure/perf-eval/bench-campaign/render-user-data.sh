#!/usr/bin/env bash
# Renders the EC2 user-data for a bench-campaign box.
#
# The scripts ship verbatim; parameters travel in a preamble of exports, so the
# bytes that run on the box match the bytes in git. Order is:
#   preamble exports -> bootstrap-common.sh -> run-campaign.sh
# Shell-quoted exports preserve each parameter as one value.
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
  printf 'export RUN_ID=%q\n' "$RUN_ID"
  printf 'export BUCKET=%q RESULT_KEY=%q\n' "$BUCKET" "$RESULT_KEY"
  printf 'export SELF_TERMINATE_MINUTES=%q BUDGET_MINUTES=%q\n' "$SELF_TERMINATE_MINUTES" "$BUDGET_MINUTES"
  printf 'export BENCH_TOML_B64=%q\n' "$BENCH_TOML_B64"
  printf 'export BENCH_REPO_REF=%q\n' "$BENCH_REPO_REF"
  cat "$DIR/../bootstrap-common.sh"
  cat "$DIR/run-campaign.sh"
} > "$OUT"
gzip -9 -c "$OUT" > "$OUT.gz"
RAW=$(wc -c < "$OUT")
SIZE=$(wc -c < "$OUT.gz")
echo "user-data is $RAW bytes raw, $SIZE bytes gzipped"
[ "$SIZE" -le 16384 ] || { echo "::error::gzipped user-data is $SIZE bytes, over the 16384-byte EC2 limit"; exit 1; }
