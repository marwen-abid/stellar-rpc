# shellcheck shell=bash
# shellcheck disable=SC2154  # env and helpers come from bootstrap-common.sh, concatenated above
#
# Bench-campaign leg. Concatenated after bootstrap-common.sh in the EC2 user-data,
# so it inherits its env, helpers, `set -euo pipefail` and the ERR trap. Runs as root.
# The benchmarks repo's campaign CLI owns the legs, the bundle, the tarball and the
# publish; this fragment translates its exit code into the verdict the poller reads.
LEG_TITLE="Bench campaign"

[ -n "${BENCH_TOML_B64:-}" ] || bail "BENCH_TOML_B64 unset; the workflow must render the campaign TOML into the user-data preamble"
BENCH_REPO_REF="${BENCH_REPO_REF:-main}"

# runner/bootstrap.sh uses $USER and $HOME under `set -u`; cloud-init sets neither.
export HOME=/root USER=root

# bootstrap_box is deliberately not called: the campaign CLI clones and builds
# stellar-rpc itself from the ref in the TOML. Only git is needed here.
log "installing git (for the benchmarks clone)"
apt-get install -y -qq --no-install-recommends git

# The basename lands verbatim in the bundle.
log "decoding campaign config"
printf '%s' "$BENCH_TOML_B64" | base64 -d > /root/bench-campaign.toml
log "campaign config:"
cat /root/bench-campaign.toml

# Full clone, not shallow: BENCH_REPO_REF may be a SHA.
log "cloning stellar-rpc-benchmarks at $BENCH_REPO_REF"
rm -rf /root/stellar-rpc-benchmarks
git clone https://github.com/stellar-experimental/stellar-rpc-benchmarks.git /root/stellar-rpc-benchmarks
git -C /root/stellar-rpc-benchmarks checkout "$BENCH_REPO_REF"

# The benchmarks bootstrap owns the machine (NVMe, fsync probe, AWS CLI, Go/Rust,
# native libs); a non-zero exit reaches the ERR trap and publishes a fail verdict.
# SRC_REF points its native-lib installers at the ref this campaign benchmarks.
SRC_REF=$(sed -n 's/^ref = "\(.*\)"$/\1/p' /root/bench-campaign.toml | head -1)
log "running runner/bootstrap.sh (SRC_REF=${SRC_REF:-unset})"
SRC_REF="$SRC_REF" bash /root/stellar-rpc-benchmarks/runner/bootstrap.sh

# bootstrap.sh appends these to .bashrc; this shell is not a login shell.
export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH
export CGO_CFLAGS="-I$HOME/.zstd/include -I$HOME/.rocksdb/include"
export CGO_LDFLAGS="-L$HOME/.zstd/lib -L$HOME/.rocksdb/lib"
export LD_LIBRARY_PATH="$HOME/.zstd/lib:$HOME/.rocksdb/lib"

# upload_bundle puts the results tarball and a run-info.json sidecar next to the
# result object. The verdict carries only markdown, so the notify job reads the
# sidecar as data; the tarball goes to $BUCKET because notify's role can read this
# bucket but not the results bucket. Every step is best-effort (an `if` condition
# or `|| log`) so a failed upload never reaches the ERR trap or changes the
# verdict. Sets TARBALL_KEY to the key written, or empty when no tarball reached S3.
#
# usage: upload_bundle <tarball> <bench_run_id> <results_uri>
upload_bundle() {
  local tarball="$1" bench_run_id="$2" results_uri="$3"
  TARBALL_KEY=""
  if [ -z "$BUCKET" ] || [ -z "$RESULT_KEY" ]; then
    log "WARN: BUCKET/RESULT_KEY unset; skipping the tarball and run-info.json"
    return 0
  fi
  local prefix="${RESULT_KEY%/*}"
  TARBALL_KEY="$prefix/$(basename "$tarball")"
  if ! aws s3api put-object --bucket "$BUCKET" --key "$TARBALL_KEY" \
         --content-type application/gzip --body "$tarball" >/dev/null; then
    log "WARN: tarball upload failed; notify skips the results-site ingest"
    TARBALL_KEY=""
  fi
  if ! jq -n --arg run "$RUN_ID" --arg bench "$bench_run_id" \
        --arg uri "$results_uri" --arg tar "$tarball" --arg tarkey "$TARBALL_KEY" \
        '{schemaVersion: 1, runId: $run, benchRunId: $bench, resultsUri: $uri, tarball: $tar, tarballKey: $tarkey}' \
        > /tmp/run-info.json; then
    log "WARN: could not write run-info.json; notify falls back to the result key"
    return 0
  fi
  aws s3api put-object --bucket "$BUCKET" --key "$prefix/run-info.json" \
        --content-type application/json --body /tmp/run-info.json >/dev/null \
    || log "WARN: run-info.json upload failed; notify falls back to the result key"
}

# newest_bundle_tarball prints the newest /tmp/bench-results-*.tgz, or nothing;
# a retried campaign on the same box can leave more than one behind.
newest_bundle_tarball() {
  local newest="" candidate
  for candidate in /tmp/bench-results-*.tgz; do
    [ -f "$candidate" ] || continue
    if [ -z "$newest" ] || [ "$candidate" -nt "$newest" ]; then
      newest="$candidate"
    fi
  done
  printf '%s' "$newest"
}

# The tee captures the `published:` line and the failure tail. pipefail makes the
# `if` see the runner's exit code, and the `if` keeps the ERR trap off a failure.
log "running campaign"
if (cd /root/stellar-rpc-benchmarks/runner \
    && BENCH_ROOT=/mnt/nvme/bench go run ./cmd/campaign run /root/bench-campaign.toml) \
    2>&1 | tee /tmp/campaign-console.log; then

  # Exit 0 means the runner printed `published: <dest>`.
  PUBLISHED_LINE=$(grep '^published: ' /tmp/campaign-console.log | tail -1 || true)
  [ -n "$PUBLISHED_LINE" ] || bail "campaign exited 0 but printed no 'published:' line; cannot locate the published bundle"
  RESULTS_URI="${PUBLISHED_LINE#published: }"
  RESULTS_URI="${RESULTS_URI%/}"
  BENCH_RUN_ID="${RESULTS_URI##*/}"
  TARBALL="/tmp/bench-results-${BENCH_RUN_ID}.tgz"
  log "published bundle $BENCH_RUN_ID to $RESULTS_URI"

  upload_bundle "$TARBALL" "$BENCH_RUN_ID" "$RESULTS_URI"

  # shellcheck disable=SC2016  # backticks are markdown code spans
  {
    printf '✅ **%s passed** (run `%s`)\n\n' "$LEG_TITLE" "$RUN_ID"
    printf -- '- bench run id: `%s`\n' "$BENCH_RUN_ID"
    printf -- '- results: `%s`\n' "$RESULTS_URI"
    printf -- '- tarball on the box: `%s`\n' "$TARBALL"
    printf -- '- benchmarks ref: `%s`\n' "$BENCH_REPO_REF"
    printf -- '- campaign config: `%s`\n' /root/bench-campaign.toml
  } > "$RESULTS_FILE"
  upload_result ok "$RESULTS_FILE"

  # poweroff can outrun the EXIT trap, so push the box log first.
  # shutdown-behavior=terminate turns this poweroff into a terminate, releasing
  # the box without waiting for the poll chain.
  upload_box_log
  log "campaign complete: $BENCH_RUN_ID — powering off (terminates the instance)"
  poweroff

else
  # The runner's epilogue tars and publishes the legs that finished, so a bundle
  # usually exists. Upload it before the verdict: the box self-terminates at its
  # ceiling and the NVMe bundle dies with it. No tarball or no `published:` line
  # is treated as missing, not as an error.
  TARBALL=$(newest_bundle_tarball)
  TARBALL_KEY=""
  if [ -n "$TARBALL" ]; then
    BENCH_RUN_ID=$(basename "$TARBALL" .tgz)
    BENCH_RUN_ID="${BENCH_RUN_ID#bench-results-}"
    PUBLISHED_LINE=$(grep '^published: ' /tmp/campaign-console.log | tail -1 || true)
    RESULTS_URI="${PUBLISHED_LINE#published: }"
    RESULTS_URI="${RESULTS_URI%/}"
    log "uploading the bundle a failed campaign left behind: $BENCH_RUN_ID"
    upload_bundle "$TARBALL" "$BENCH_RUN_ID" "$RESULTS_URI"
  else
    log "WARN: no /tmp/bench-results-*.tgz on the box; there is no bundle to upload"
  fi

  # shellcheck disable=SC2016  # backticks are markdown code spans
  {
    printf '❌ **%s failed** (run `%s`)\n\n' "$LEG_TITLE" "$RUN_ID"
    printf 'Last 60 lines of the campaign console:\n\n```\n'
    tail -n 60 /tmp/campaign-console.log
    printf '```\n\n'
    if [ -n "$TARBALL_KEY" ]; then
      printf 'Uploaded bundle: `s3://%s/%s`, holding the legs that finished before the failure.\n' "$BUCKET" "$TARBALL_KEY"
    else
      printf 'No bundle uploaded: an SSM rescue is the only way to reach the results.\n'
    fi
    printf 'Rescue: the bundle is under `/mnt/nvme/bench/results/`, the tarball at `/tmp/bench-results-*.tgz`.\n'
    printf 'The box is left running until its self-terminate ceiling (%s minutes after boot), so an operator can SSM in.\n' "$SELF_TERMINATE_MINUTES"
  } > "$RESULTS_FILE"
  upload_result fail "$RESULTS_FILE"

  # Deliberately no poweroff: the bundle may exist only on this box, so it stays up
  # for rescue until the self-terminate ceiling (and the reaper).
  log "campaign failed; leaving the box up for rescue until the self-terminate ceiling"
  exit 1
fi
