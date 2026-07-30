#!/usr/bin/env bash
# spike/aws.sh — pier AWS spike: verify the load-bearing bets with throwaway
# resources. Everything is named pier-spike and removed on exit.
#
# Verifies (see docs/SPEC.md §14):
#   1. boot -> SSM-ready time on Ubuntu 24.04 arm64   (attach-latency bet)
#   2. SSM send-command round-trip                    (API-path upper bound)
#   3. in-VM `shutdown -h now` + shutdown-behavior=stop lands in `stopped`,
#      NOT terminated                                 (credential-free self-park bet)
#   4. stop -> start -> command-ready time            (resume bet)
#
# Usage:
#   ./aws.sh            run all measurements, then clean up
#   ./aws.sh --keep     same, but leave everything up for the manual TTY test:
#                         aws ssm start-session --target <instance-id>
#   ./aws.sh --cleanup  remove leftovers from a --keep or crashed run
#
# Cost of a run: ~$0.01 (t4g.medium for ~10 minutes).
set -euo pipefail

NAME=pier-spike
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || true)}"
REGION="${REGION:-eu-central-1}"
export AWS_DEFAULT_REGION="$REGION" AWS_PAGER=""
AMI_PARAM=/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id

MODE=run
case "${1:-}" in
  --keep) MODE=keep ;;
  --cleanup) MODE=cleanup ;;
esac

IID=""
log() { echo "[$(date +%H:%M:%S)] $*"; }
now() { date +%s; }

spike_instances() {
  aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=$NAME" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text
}

cleanup() {
  set +e
  log "cleanup: removing $NAME resources in $REGION"
  local ids sg i
  ids=$(spike_instances)
  if [ -n "$ids" ] && [ "$ids" != "None" ]; then
    aws ec2 terminate-instances --instance-ids $ids >/dev/null 2>&1
    log "cleanup: waiting for instance(s) to terminate (frees the SG)"
    aws ec2 wait instance-terminated --instance-ids $ids >/dev/null 2>&1
  fi
  sg=$(aws ec2 describe-security-groups --filters "Name=group-name,Values=$NAME" \
       --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null)
  if [ -n "$sg" ] && [ "$sg" != "None" ]; then
    for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
      aws ec2 delete-security-group --group-id "$sg" >/dev/null 2>&1 && break
      sleep 5
    done
  fi
  aws iam remove-role-from-instance-profile --instance-profile-name "$NAME" --role-name "$NAME" >/dev/null 2>&1
  aws iam delete-instance-profile --instance-profile-name "$NAME" >/dev/null 2>&1
  aws iam detach-role-policy --role-name "$NAME" \
    --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore >/dev/null 2>&1
  aws iam delete-role --role-name "$NAME" >/dev/null 2>&1
  log "cleanup: done"
}

if [ "$MODE" = cleanup ]; then cleanup; exit 0; fi
if [ "$MODE" = run ]; then trap cleanup EXIT; fi

# run_cmd CMD -> echoes round-trip seconds; fails if the command can't be
# delivered or doesn't succeed within 60s.
run_cmd() {
  local t0 cid status
  t0=$(now)
  cid=$(aws ssm send-command --instance-ids "$IID" --document-name AWS-RunShellScript \
        --parameters commands="$1" --query Command.CommandId --output text 2>/dev/null) || return 1
  while :; do
    status=$(aws ssm get-command-invocation --command-id "$cid" --instance-id "$IID" \
             --query Status --output text 2>/dev/null || echo Pending)
    case "$status" in
      Success) echo $(( $(now) - t0 )); return 0 ;;
      Failed|Cancelled|TimedOut|Undeliverable|Terminated) return 1 ;;
    esac
    [ $(( $(now) - t0 )) -gt 60 ] && return 1
    sleep 2
  done
}

ACCT=$(aws sts get-caller-identity --query Account --output text)
log "account $ACCT, region $REGION"

log "groundwork: role + instance profile + egress-only SG (all '$NAME')"
if ! aws iam get-role --role-name "$NAME" >/dev/null 2>&1; then
  aws iam create-role --role-name "$NAME" --assume-role-policy-document \
    '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
fi
aws iam attach-role-policy --role-name "$NAME" \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
if ! aws iam get-instance-profile --instance-profile-name "$NAME" >/dev/null 2>&1; then
  aws iam create-instance-profile --instance-profile-name "$NAME" >/dev/null
fi
aws iam add-role-to-instance-profile --instance-profile-name "$NAME" --role-name "$NAME" 2>/dev/null || true

VPC=$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
if [ "$VPC" = "None" ] || [ -z "$VPC" ]; then log "FAIL: no default VPC in $REGION"; exit 1; fi
SG=$(aws ec2 describe-security-groups --filters "Name=group-name,Values=$NAME" "Name=vpc-id,Values=$VPC" \
     --query 'SecurityGroups[0].GroupId' --output text)
if [ "$SG" = "None" ] || [ -z "$SG" ]; then
  SG=$(aws ec2 create-security-group --group-name "$NAME" --vpc-id "$VPC" \
       --description "pier spike - egress only, zero inbound" --query GroupId --output text)
fi
log "groundwork: vpc=$VPC sg=$SG (no inbound rules)"

AMI=$(aws ssm get-parameter --name "$AMI_PARAM" --query Parameter.Value --output text)
log "ami: $AMI (ubuntu 24.04 arm64, resolved via SSM parameter)"

log "launching t4g.medium with --instance-initiated-shutdown-behavior stop ..."
T0=$(now)
for i in 1 2 3 4 5 6 7 8 9 10; do
  set +e
  OUT=$(aws ec2 run-instances \
    --image-id "$AMI" --instance-type t4g.medium \
    --iam-instance-profile "Name=$NAME" \
    --security-group-ids "$SG" \
    --instance-initiated-shutdown-behavior stop \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$NAME}]" \
    --query 'Instances[0].InstanceId' --output text 2>&1)
  rc=$?
  set -e
  if [ $rc -eq 0 ]; then IID=$OUT; break; fi
  if echo "$OUT" | grep -qi 'invalid iam instance profile\|iaminstanceprofile'; then
    log "  IAM still propagating, retry $i/10"
    sleep 5
  else
    log "FAIL: run-instances: $OUT"
    exit 1
  fi
done
if [ -z "$IID" ]; then log "FAIL: IAM instance profile never propagated"; exit 1; fi
log "instance: $IID"

log "waiting for SSM agent to come online (attach-ready proxy) ..."
while :; do
  PING=$(aws ssm describe-instance-information \
    --filters "Key=InstanceIds,Values=$IID" \
    --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || echo None)
  [ "$PING" = "Online" ] && break
  if [ $(( $(now) - T0 )) -gt 300 ]; then log "FAIL: SSM not online after 300s"; exit 1; fi
  sleep 3
done
BOOT_SSM=$(( $(now) - T0 ))
log "PASS: launch -> SSM online in ${BOOT_SSM}s"

log "measuring send-command round-trips (x3) ..."
R1=$(run_cmd "echo pong") || R1=fail
R2=$(run_cmd "echo pong") || R2=fail
R3=$(run_cmd "echo pong") || R3=fail
log "send-command RTT: ${R1}s / ${R2}s / ${R3}s (polling upper bound; interactive TTY is snappier)"

log "park test: in-VM 'sudo shutdown -h now' (the credential-free self-park)"
aws ssm send-command --instance-ids "$IID" --document-name AWS-RunShellScript \
  --parameters commands="sudo shutdown -h now" >/dev/null
TP=$(now)
while :; do
  STATE=$(aws ec2 describe-instances --instance-ids "$IID" \
    --query 'Reservations[0].Instances[0].State.Name' --output text)
  case "$STATE" in stopped|terminated) break ;; esac
  if [ $(( $(now) - TP )) -gt 300 ]; then log "FAIL: still '$STATE' 300s after shutdown"; exit 1; fi
  sleep 3
done
PARK=$(( $(now) - TP ))
if [ "$STATE" = "stopped" ]; then
  log "PASS: shutdown -h now -> stopped in ${PARK}s (disk intact — park bet holds)"
else
  log "FAIL: shutdown -h now -> '$STATE' (expected stopped) — park bet BROKEN"
  exit 1
fi

log "resume test: start-instances -> first successful command"
aws ec2 start-instances --instance-ids "$IID" >/dev/null
TR=$(now)
while :; do
  if R=$(run_cmd "true"); then RESUME=$(( $(now) - TR )); break; fi
  if [ $(( $(now) - TR )) -gt 300 ]; then log "FAIL: not command-ready 300s after start"; exit 1; fi
  sleep 2
done
log "PASS: parked -> command-ready in ${RESUME}s"

echo
echo "==== RESULTS ($REGION, t4g.medium, ubuntu 24.04 arm64) ===="
echo "  launch -> SSM online:      ${BOOT_SSM}s   (cold-create floor; target attach 60-90s)"
echo "  send-command RTT:          ${R1}s / ${R2}s / ${R3}s"
echo "  shutdown -h now:           stopped in ${PARK}s   (self-park bet: PASS)"
echo "  start -> command-ready:    ${RESUME}s   (resume target ~30s)"
echo "==========================================================="
echo

if [ "$MODE" = keep ]; then
  echo "instance left RUNNING for the manual TTY-feel test:"
  echo "  aws ssm start-session --target $IID"
  echo "clean up afterwards with: $0 --cleanup"
fi
