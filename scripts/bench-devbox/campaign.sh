#!/usr/bin/env bash
#
# Full benchmark campaign on real pubnet chunks (dev box, NVMe). Rebuilds the
# current checkout, then runs the golden-pack ingest (S3, only for chunks not
# already frozen on the NVMe), the timed cold/hot ingest repetitions, and the
# cold/hot query sweeps. Every run is a fresh process with its own --out dir.
#
# Results land in /mnt/nvme/bench/results/<git-sha>-<utc-stamp>/ and are
# bundled to /tmp/bench-results-<git-sha>-<utc-stamp>.tgz (the EBS root, so
# the bundle survives an instance stop).
#
# Usage (on the box, inside tmux — a campaign takes a while):
#   ./scripts/bench-devbox/campaign.sh
# or detached:
#   nohup ./scripts/bench-devbox/campaign.sh > ~/campaign.log 2>&1 &
#
# Overridable: CHUNKS ("3000 5000 6100 6345"), RUNS (5), QC ("1,4,16"),
# COLD_ITERS (100), HOT_ITERS (200), REPO (~/stellar-rpc).
# To force re-downloading a golden pack: rm -rf /mnt/nvme/bench/golden/<chunk>.
#
set -euo pipefail

CHUNKS="${CHUNKS:-3000 5000 6100 6345}"
RUNS="${RUNS:-5}"
QC="${QC:-1,4,16}"
COLD_ITERS="${COLD_ITERS:-100}"
HOT_ITERS="${HOT_ITERS:-200}"
REPO="${REPO:-$HOME/stellar-rpc}"
BENCH=/mnt/nvme/bench

note() { echo "== [$(date -u +%H:%M:%S)] $*"; }

mountpoint -q /mnt/nvme || { echo "error: /mnt/nvme not mounted — run bootstrap.sh first" >&2; exit 1; }

# --- rebuild the current checkout so the campaign measures what's checked out --
cd "$REPO"
note "make install ($(git rev-parse --abbrev-ref HEAD))"
make install
SHA=$(git describe --always --dirty --abbrev=8)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
RES="$BENCH/results/$SHA-$STAMP"
mkdir -p "$RES"
note "results → $RES"

# Without this the AWS SDK signs S3 requests with the box's IAM role and the
# public bucket 403s; with no creds it falls back to anonymous access.
export AWS_EC2_METADATA_DISABLED=true

# --- golden packs: download from S3 only if not already frozen -----------------
for C in $CHUNKS; do
  if [ ! -d "$BENCH/golden/$C" ]; then
    note "golden download chunk $C (S3)"
    stellar-rpc bench-ingest cold \
      --source=bsb --datastore-type=S3 --region=us-east-2 \
      --bucket-path=aws-public-blockchain/v1.1/stellar/ledgers/pubnet \
      --start-chunk="$C" --num-chunks=1 \
      --cold-out-dir="$BENCH/golden/$C" \
      --out="$RES/golden-download-$C"
  fi
done

# --- timed cold ingest: RUNS fresh processes per chunk, from the golden pack ---
for C in $CHUNKS; do
  for R in $(seq 1 "$RUNS"); do
    note "ingest-cold chunk $C run $R/$RUNS"
    rm -rf "$BENCH/scratch/$C"
    stellar-rpc bench-ingest cold \
      --source=pack --pack-dir="$BENCH/golden/$C/ledgers" \
      --start-chunk="$C" --num-chunks=1 \
      --cold-out-dir="$BENCH/scratch/$C" \
      --out="$RES/ingest-cold-$C-run$R"
  done
done

# --- timed hot ingest: DB deleted before each run; the last DB is kept ---------
# (bench-query hot below needs it)
for C in $CHUNKS; do
  for R in $(seq 1 "$RUNS"); do
    note "ingest-hot chunk $C run $R/$RUNS"
    rm -rf "$BENCH/hot/$C"
    stellar-rpc bench-ingest hot \
      --source=pack --pack-dir="$BENCH/golden/$C/ledgers" \
      --start-chunk="$C" \
      --hot-dir="$BENCH/hot/$C" \
      --out="$RES/ingest-hot-$C-run$R"
  done
done

# --- query sweeps: cold evicts the page cache per iteration (Linux-only) -------
for C in $CHUNKS; do
  for R in $(seq 1 "$RUNS"); do
    note "query-cold chunk $C run $R/$RUNS"
    stellar-rpc bench-query cold \
      --cold-dir="$BENCH/golden/$C" --start-chunk="$C" --num-chunks=1 \
      --types=ledgers,txpage,txhash,events \
      --query-concurrency="$QC" --iters="$COLD_ITERS" \
      --out="$RES/query-cold-$C-run$R"
  done
done

for C in $CHUNKS; do
  for R in $(seq 1 "$RUNS"); do
    note "query-hot chunk $C run $R/$RUNS"
    stellar-rpc bench-query hot \
      --hot-dir="$BENCH/hot/$C" --chunk="$C" \
      --types=ledgers,txpage,txhash,events \
      --query-concurrency="$QC" --iters="$HOT_ITERS" --warmup=20 \
      --out="$RES/query-hot-$C-run$R"
  done
done

# --- machine metadata ----------------------------------------------------------
note "machine metadata"
{
  date -u
  TOKEN=$(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
  echo "instance-type: $(curl -sH "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-type)"
  echo "instance-id:   $(curl -sH "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)"
  uname -a; lsb_release -ds
  lscpu | grep -E 'Model name|^CPU\(s\)'
  free -h | head -2
  lsblk -o NAME,SIZE,MODEL
  echo "repo: $(git -C "$REPO" rev-parse HEAD) ($(git -C "$REPO" rev-parse --abbrev-ref HEAD), describe $SHA)"
  stellar-rpc version 2>&1 | head -3
  go version; "$HOME/.cargo/bin/rustc" --version
  echo "chunks: $CHUNKS · runs: $RUNS · concurrency: $QC · cold-iters: $COLD_ITERS · hot-iters: $HOT_ITERS"
  echo -n "fsync probe: "
  dd if=/dev/zero of=/mnt/nvme/.fp bs=4k count=2000 oflag=dsync 2>&1 | tail -1; rm -f /mnt/nvme/.fp
} > "$RES/machine-metadata.txt" 2>&1

# --- bundle to the EBS root so it survives an instance stop ---------------------
TARBALL="/tmp/bench-results-$SHA-$STAMP.tgz"
tar -C "$BENCH/results" -czf "$TARBALL" "$SHA-$STAMP"
note "campaign done: $TARBALL"
