#!/usr/bin/env bash
# One-shot: cross-compile the gateway, copy it to the facility Pi, configure it
# for prod, and start it. Run this FROM a machine on the FACILITY LAN (it reaches
# the Pi over mDNS at cannabits-gw.local — off-site that name will not resolve).
#
#   CANNABITS_API_KEY=cbk_... PLAN_ID=<uuid> ./deploy-to-pi.sh [pi-host]
#
# Secrets are passed via env, never committed here.
set -euo pipefail
HOST="${1:-cannabits-gw.local}"
PI_USER="${PI_USER:-pi}"
API_URL="${CANNABITS_API_URL:-https://growgoddess.club}"
: "${CANNABITS_API_KEY:?set CANNABITS_API_KEY (the prod gateway key)}"
: "${PLAN_ID:?set PLAN_ID (the prod plan uuid)}"
here="$(cd "$(dirname "$0")" && pwd)"
target="$PI_USER@$HOST"

echo "==> Pi architecture"
arch=$(ssh -o ConnectTimeout=6 "$target" 'uname -m')
case "$arch" in
  aarch64|arm64) goarch=arm64; buildarg=arm64 ;;
  armv7l|armv6l) goarch=arm;   buildarg=arm   ;;
  *) echo "unexpected arch '$arch'"; exit 1 ;;
esac
echo "    $arch -> building $buildarg"
( cd "$here" && ./build.sh "$buildarg" )   # writes dist/cannabits-gateway

echo "==> copy repo + prebuilt binary to $target"
ssh "$target" 'mkdir -p ~/growgoddess-pi'
rsync -az --delete --exclude .git "$here/" "$target:~/growgoddess-pi/"

echo "==> install, configure for prod, start"
ssh "$target" "cd ~/growgoddess-pi && sudo ./install.sh
sudo tee /etc/cannabits/gateway.env >/dev/null <<EOF
CANNABITS_API_URL=$API_URL
CANNABITS_API_KEY=$CANNABITS_API_KEY
EOF
sudo chmod 600 /etc/cannabits/gateway.env
sudo python3 - <<PY
import json
p='/etc/cannabits/gateway.json'
d=json.load(open(p)); d['planId']='$PLAN_ID'; d['gatewayLabel']='cannaru-pi'
open(p,'w').write(json.dumps(d, indent=2))
PY
sudo systemctl restart cannabits-gateway
sleep 4
sudo journalctl -u cannabits-gateway -n 20 --no-pager"

echo
echo "==> If you see 'sent ...: N stored' above, it is live on $API_URL."
echo "    Follow it:  ssh $target 'journalctl -u cannabits-gateway -f'"
