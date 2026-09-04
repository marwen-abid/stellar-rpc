#!/usr/bin/env bash
# Launches the campaign box from the rendered user-data.
# Env: INSTANCE_TYPE, CAMPAIGN_NAME, SELF_TERMINATE_MINUTES, RUN_ID_TAG,
# USER_DATA (default /tmp/user-data.sh.gz, what render-user-data.sh writes).
# Output: instance_id, appended to GITHUB_OUTPUT (stdout when unset).
set -euo pipefail

# rpc-bench + deadline are what bench-reaper.yml sweeps on; the test tag is what
# the GHA role's terminate permission is conditioned on. The deadline tag is the
# reaper's backstop: past the self-terminate ceiling so a failed campaign keeps
# its rescue window, computed at launch so queue delay cannot shrink it, +30 min
# for launch-to-boot lag (the box's shutdown clock starts at boot).
REAPER_DEADLINE_EPOCH=$(( $(date +%s) + (SELF_TERMINATE_MINUTES + 30) * 60 ))
COMMON_TAGS="{Key=test,Value=stellar-rpc-ci-load-test},
  {Key=rpc-bench,Value=true},
  {Key=run-id,Value=$RUN_ID_TAG},
  {Key=deadline,Value=$REAPER_DEADLINE_EPOCH}"
# AMI: Canonical Ubuntu 24.04 LTS (Noble) amd64, us-east-1, gp3, serial 20260714;
# the benchmarks runner's bootstrap.sh requires 24.04. Root gp3 stays small
# because datasets, RocksDB and packfiles live on the NVMe instance store.
RUN_INSTANCES_JSON=$(aws ec2 run-instances \
  --image-id ami-052355af2a014bd2c \
  --instance-type "$INSTANCE_TYPE" \
  --iam-instance-profile Name=stellar-rpc-ci-load-test \
  --user-data fileb://"${USER_DATA:-/tmp/user-data.sh.gz}" \
  --instance-initiated-shutdown-behavior terminate \
  --block-device-mappings '[{
    "DeviceName":"/dev/sda1",
    "Ebs":{"VolumeSize":50,"VolumeType":"gp3","Iops":3000,"Throughput":125,"DeleteOnTermination":true}
  }]' \
  --tag-specifications \
    "ResourceType=instance,Tags=[
      {Key=Name,Value=bench-campaign-$CAMPAIGN_NAME},
      $COMMON_TAGS
    ]" \
    "ResourceType=volume,Tags=[
      {Key=Name,Value=bench-campaign-$CAMPAIGN_NAME-root},
      $COMMON_TAGS
    ]" \
  --count 1 \
  --output json)

INSTANCE_ID=$(printf '%s' "$RUN_INSTANCES_JSON" | jq -r '.Instances[0].InstanceId')
echo "instance_id=$INSTANCE_ID" >> "${GITHUB_OUTPUT:-/dev/stdout}"
echo "launched $INSTANCE_ID ($INSTANCE_TYPE) for campaign $CAMPAIGN_NAME"
