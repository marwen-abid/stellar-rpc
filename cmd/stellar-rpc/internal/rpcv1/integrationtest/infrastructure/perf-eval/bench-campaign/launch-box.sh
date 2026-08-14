#!/usr/bin/env bash
set -euo pipefail
# Launches the tagged EC2 campaign box from rendered user-data.
# Env: INSTANCE_TYPE, CAMPAIGN_NAME, SELF_TERMINATE_MINUTES, RUN_ID_TAG, USER_DATA.
# Output: instance_id is appended to GITHUB_OUTPUT, or stdout when unset.

# rpc-bench + deadline are what bench-reaper.yml sweeps on; the test tag is
# what the GHA role's terminate permission is conditioned on. The deadline tag
# is computed at launch so queue delay cannot shrink it, and sits 30 minutes
# past the self-terminate ceiling to cover launch-to-boot lag.
REAPER_DEADLINE_EPOCH=$(( $(date +%s) + (SELF_TERMINATE_MINUTES + 30) * 60 ))
COMMON_TAGS="{Key=test,Value=stellar-rpc-ci-load-test},
  {Key=rpc-bench,Value=true},
  {Key=run-id,Value=$RUN_ID_TAG},
  {Key=deadline,Value=$REAPER_DEADLINE_EPOCH}"
# AMI: Canonical Ubuntu 24.04 LTS (Noble) amd64, us-east-1, gp3, serial
# 20260714. The benchmarks runner's bootstrap.sh requires 24.04.
# Root gp3 stays small because datasets, RocksDB, and packfiles use NVMe.
RUN_INSTANCES_JSON=$(aws ec2 run-instances \
  --image-id ami-052355af2a014bd2c \
  --instance-type "$INSTANCE_TYPE" \
  --iam-instance-profile Name=stellar-rpc-ci-load-test \
  --user-data file://"${USER_DATA:-/tmp/user-data.sh}" \
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
