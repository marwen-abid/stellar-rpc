# shellcheck shell=bash
# shellcheck disable=SC2154
# Concatenated after bootstrap-common.sh in EC2 user-data, this root fragment
# inherits its env, helpers, active `set -euo pipefail`, and ERR trap.
# The campaign CLI owns the campaign; this translates its exit into a verdict.
LEG_TITLE="Bench campaign"

[ -n "${BENCH_TOML_B64:-}" ] || bail "BENCH_TOML_B64 unset; the workflow must render the campaign TOML into the user-data preamble"
BENCH_REPO_REF="${BENCH_REPO_REF:-main}"

# The benchmarks bootstrap uses HOME and USER under `set -u`; cloud-init sets
# neither.
export HOME=/root USER=root

# bootstrap_box is not called: the campaign CLI clones and builds stellar-rpc;
# this box only needs git.
log "installing git (for the benchmarks clone)"
apt-get install -y -qq --no-install-recommends git

# This basename appears verbatim in the result bundle.
log "decoding campaign config"
printf '%s' "$BENCH_TOML_B64" | base64 -d > /root/bench-campaign.toml
log "campaign config:"
cat /root/bench-campaign.toml

# BENCH_REPO_REF may be a SHA, so this must be a full clone.
log "cloning stellar-rpc-benchmarks at $BENCH_REPO_REF"
rm -rf /root/stellar-rpc-benchmarks
git clone https://github.com/stellar-experimental/stellar-rpc-benchmarks.git /root/stellar-rpc-benchmarks
git -C /root/stellar-rpc-benchmarks checkout "$BENCH_REPO_REF"

# SRC_REF makes the bootstrap use the benchmarked ref's native-library pins.
# Bootstrap failures reach the active ERR trap and publish a fail verdict.
SRC_REF=$(sed -n 's/^ref = "\(.*\)"$/\1/p' /root/bench-campaign.toml | head -1)
log "running runner/bootstrap.sh (SRC_REF=${SRC_REF:-unset})"
SRC_REF="$SRC_REF" bash /root/stellar-rpc-benchmarks/runner/bootstrap.sh

# These mirror runner/bootstrap.sh's .bashrc exports, which this non-login
# shell never reads.
export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH
export CGO_CFLAGS="-I$HOME/.zstd/include -I$HOME/.rocksdb/include"
export CGO_LDFLAGS="-L$HOME/.zstd/lib -L$HOME/.rocksdb/lib"
export LD_LIBRARY_PATH="$HOME/.zstd/lib:$HOME/.rocksdb/lib"

# With pipefail, this `if` sees the runner's exit code and routes failure below
# without invoking the ERR trap.
log "running campaign"
if (cd /root/stellar-rpc-benchmarks/runner \
    && BENCH_ROOT=/mnt/nvme/bench go run ./cmd/campaign run /root/bench-campaign.toml) \
    2>&1 | tee /tmp/campaign-console.log; then

  # A successful runner prints `published: <dest>`.
  PUBLISHED_LINE=$(grep '^published: ' /tmp/campaign-console.log | tail -1 || true)
  [ -n "$PUBLISHED_LINE" ] || bail "campaign exited 0 but printed no 'published:' line; cannot locate the published bundle"
  RESULTS_URI="${PUBLISHED_LINE#published: }"
  RESULTS_URI="${RESULTS_URI%/}"
  BENCH_RUN_ID="${RESULTS_URI##*/}"
  TARBALL="/tmp/bench-results-${BENCH_RUN_ID}.tgz"
  log "published bundle $BENCH_RUN_ID to $RESULTS_URI"

  # Sidecar uploads are best-effort: failures must not change a passed verdict.
  # Notify falls back to the result key, and no tarball key skips site ingest.
  # The tarball goes to the run's own prefix because the notify job's role can
  # read this bucket but not the results bucket; it is what notify ingests.
  if [ -n "$BUCKET" ] && [ -n "$RESULT_KEY" ]; then
    TARBALL_KEY="${RESULT_KEY%/*}/$(basename "$TARBALL")"
    if ! aws s3api put-object --bucket "$BUCKET" --key "$TARBALL_KEY" \
           --content-type application/gzip --body "$TARBALL" >/dev/null; then
      log "WARN: tarball upload failed; notify skips the results-site ingest"
      TARBALL_KEY=""
    fi
    jq -n --arg run "$RUN_ID" --arg bench "$BENCH_RUN_ID" \
          --arg uri "$RESULTS_URI" --arg tar "$TARBALL" --arg tarkey "$TARBALL_KEY" \
          '{schemaVersion: 1, runId: $run, benchRunId: $bench, resultsUri: $uri, tarball: $tar, tarballKey: $tarkey}' \
          > /tmp/run-info.json
    aws s3api put-object --bucket "$BUCKET" --key "${RESULT_KEY%/*}/run-info.json" \
          --content-type application/json --body /tmp/run-info.json >/dev/null \
      || log "WARN: run-info.json upload failed; notify falls back to the result key"
  else
    log "WARN: BUCKET/RESULT_KEY unset; skipping run-info.json"
  fi

  # shellcheck disable=SC2016  # the backticks below are markdown code spans, not shell expansion
  {
    printf '✅ **%s passed** (run `%s`)\n\n' "$LEG_TITLE" "$RUN_ID"
    printf -- '- bench run id: `%s`\n' "$BENCH_RUN_ID"
    printf -- '- results: `%s`\n' "$RESULTS_URI"
    printf -- '- tarball on the box: `%s`\n' "$TARBALL"
    printf -- '- benchmarks ref: `%s`\n' "$BENCH_REPO_REF"
    printf -- '- campaign config: `%s`\n' /root/bench-campaign.toml
  } > "$RESULTS_FILE"
  upload_result ok "$RESULTS_FILE"

  # Poweroff can outrun the EXIT trap, so upload first. EC2 shutdown-behavior
  # terminates and frees the passing box.
  upload_box_log
  log "campaign complete: $BENCH_RUN_ID — powering off (terminates the instance)"
  poweroff

else
  # The runner's epilogue leaves a failed campaign's bundle available to rescue.
  # shellcheck disable=SC2016  # the backticks below are markdown code spans, not shell expansion
  {
    printf '❌ **%s failed** (run `%s`)\n\n' "$LEG_TITLE" "$RUN_ID"
    printf 'Last 60 lines of the campaign console:\n\n```\n'
    tail -n 60 /tmp/campaign-console.log
    printf '```\n\n'
    printf 'Rescue: the bundle is under `/mnt/nvme/bench/results/`, the tarball at `/tmp/bench-results-*.tgz`.\n'
    printf 'The box is left running until its self-terminate ceiling (%s minutes after boot), so an operator can SSM in.\n' "$SELF_TERMINATE_MINUTES"
  } > "$RESULTS_FILE"
  upload_result fail "$RESULTS_FILE"

  # Do not power off: the failed box stays available for SSM rescue until its
  # self-terminate ceiling.
  log "campaign failed; leaving the box up for rescue until the self-terminate ceiling"
  exit 1
fi
