#!/usr/bin/env bash
# spike/gcp.sh — pier GCE spike. *** STATUS: UNTESTED ***
# Written alongside aws.sh; run when a GCP project is at hand and record the
# numbers in spike/README.md.
#
# Verifies (see docs/SPEC.md §14):
#   1. boot -> IAP-ssh-ready time on Ubuntu 24.04     (attach-latency bet)
#   2. in-VM `shutdown -h now` lands in TERMINATED with the boot disk intact
#      and the instance restartable                   (credential-free self-park bet)
#   3. start -> ssh-ready time                        (resume bet)
#
# Usage: ./gcp.sh [--keep|--cleanup]     (same modes as aws.sh)
# Notes: first-ever `gcloud compute ssh` in a project generates and propagates
# an SSH key — that run reads slow; the steady-state number is the honest one.
set -euo pipefail

NAME=pier-spike
ZONE="${ZONE:-europe-west3-a}"
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}"
if [ -z "$PROJECT" ]; then echo "FAIL: no gcloud project (set PROJECT= or gcloud config)"; exit 1; fi
GC="gcloud --project=$PROJECT"

MODE=run
case "${1:-}" in
  --keep) MODE=keep ;;
  --cleanup) MODE=cleanup ;;
esac

log() { echo "[$(date +%H:%M:%S)] $*"; }
now() { date +%s; }

cleanup() {
  set +e
  log "cleanup: removing $NAME resources in $PROJECT/$ZONE"
  $GC compute instances delete "$NAME" --zone "$ZONE" --quiet >/dev/null 2>&1
  $GC compute firewall-rules delete "$NAME-iap" --quiet >/dev/null 2>&1
  log "cleanup: done"
}

if [ "$MODE" = cleanup ]; then cleanup; exit 0; fi
if [ "$MODE" = run ]; then trap cleanup EXIT; fi

# ssh_ok -> 0 if an IAP-tunneled command runs; each attempt is its own probe.
ssh_ok() {
  $GC compute ssh "$NAME" --zone "$ZONE" --tunnel-through-iap \
    --command true --ssh-flag="-o ConnectTimeout=5" >/dev/null 2>&1
}

log "groundwork: IAP firewall rule ($NAME-iap)"
if ! $GC compute firewall-rules describe "$NAME-iap" >/dev/null 2>&1; then
  $GC compute firewall-rules create "$NAME-iap" \
    --direction=INGRESS --action=ALLOW --rules=tcp:22 \
    --source-ranges=35.235.240.0/20 --target-tags="$NAME" >/dev/null
fi

log "launching e2-medium (ubuntu 24.04, no service account, no scopes) ..."
T0=$(now)
$GC compute instances create "$NAME" --zone "$ZONE" \
  --machine-type e2-medium \
  --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud \
  --no-service-account --no-scopes \
  --tags "$NAME" >/dev/null

log "waiting for IAP ssh to answer (attach-ready proxy) ..."
while ! ssh_ok; do
  if [ $(( $(now) - T0 )) -gt 300 ]; then log "FAIL: ssh not ready after 300s"; exit 1; fi
  sleep 3
done
BOOT_SSH=$(( $(now) - T0 ))
log "PASS: create -> IAP ssh ready in ${BOOT_SSH}s"

log "park test: in-VM 'sudo shutdown -h now'"
$GC compute ssh "$NAME" --zone "$ZONE" --tunnel-through-iap \
  --command "sudo shutdown -h now" >/dev/null 2>&1 || true
TP=$(now)
while :; do
  ST=$($GC compute instances describe "$NAME" --zone "$ZONE" --format 'value(status)' 2>/dev/null || echo GONE)
  [ "$ST" = "TERMINATED" ] && break
  [ "$ST" = "GONE" ] && { log "FAIL: instance disappeared — park bet BROKEN"; exit 1; }
  if [ $(( $(now) - TP )) -gt 300 ]; then log "FAIL: still '$ST' 300s after shutdown"; exit 1; fi
  sleep 3
done
PARK=$(( $(now) - TP ))
DISK=$($GC compute instances describe "$NAME" --zone "$ZONE" --format 'value(disks[0].source)')
if [ -n "$DISK" ]; then
  log "PASS: shutdown -> TERMINATED in ${PARK}s, boot disk intact (park bet holds)"
else
  log "FAIL: TERMINATED but no disk attached — park bet BROKEN"; exit 1
fi

log "resume test: instances start -> ssh-ready"
$GC compute instances start "$NAME" --zone "$ZONE" >/dev/null
TR=$(now)
while ! ssh_ok; do
  if [ $(( $(now) - TR )) -gt 300 ]; then log "FAIL: ssh not ready 300s after start"; exit 1; fi
  sleep 3
done
RESUME=$(( $(now) - TR ))
log "PASS: parked -> ssh-ready in ${RESUME}s"

echo
echo "==== RESULTS ($PROJECT/$ZONE, e2-medium, ubuntu 24.04) ===="
echo "  create -> IAP ssh ready:  ${BOOT_SSH}s   (cold-create floor; target attach 60-90s)"
echo "  shutdown -h now:          TERMINATED in ${PARK}s, disk intact (self-park: PASS)"
echo "  start -> ssh-ready:       ${RESUME}s   (resume target ~30s)"
echo "==========================================================="
echo

if [ "$MODE" = keep ]; then
  echo "instance left RUNNING for the manual TTY-feel test:"
  echo "  gcloud --project=$PROJECT compute ssh $NAME --zone $ZONE --tunnel-through-iap"
  echo "clean up afterwards with: $0 --cleanup"
fi
