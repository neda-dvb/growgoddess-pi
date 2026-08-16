#!/usr/bin/env bash
# Install the Cannabits Edge Gateway on a Raspberry Pi. Run ON THE PI as root:
#   sudo ./install.sh
#
# It installs the binary, a systemd service, and config templates, then leaves
# the service enabled but stopped so you can fill in the key before it starts.
set -euo pipefail

BIN=/usr/local/bin/cannabits-gateway
ETC=/etc/cannabits
SPOOL=/var/lib/cannabits-gateway/spool
UNIT=/etc/systemd/system/cannabits-gateway.service
here="$(cd "$(dirname "$0")" && pwd)"

if [[ $EUID -ne 0 ]]; then echo "run as root: sudo ./install.sh"; exit 1; fi

echo "==> gateway binary"
if [[ -x "$here/dist/cannabits-gateway" ]]; then
  install -m 0755 "$here/dist/cannabits-gateway" "$BIN"
  echo "    installed prebuilt $BIN"
elif command -v go >/dev/null 2>&1; then
  ( cd "$here" && go build -trimpath -o "$BIN" ./cmd/gateway )
  echo "    built and installed $BIN"
else
  echo "    no dist/cannabits-gateway and no Go toolchain on this Pi."
  echo "    On your Mac:  ./build.sh   then copy this folder to the Pi and re-run."
  exit 1
fi

echo "==> config"
install -d -m 0755 "$ETC"
install -d -m 0755 "$SPOOL"
if [[ ! -f "$ETC/gateway.json" ]]; then
  install -m 0644 "$here/gateway.example.json" "$ETC/gateway.json"
  echo "    wrote $ETC/gateway.json (edit planId + gatewayLabel)"
fi
if [[ ! -f "$ETC/gateway.env" ]]; then
  install -m 0600 "$here/gateway.env.example" "$ETC/gateway.env"
  echo "    wrote $ETC/gateway.env (edit CANNABITS_API_KEY)"
fi

echo "==> systemd service"
install -m 0644 "$here/systemd/cannabits-gateway.service" "$UNIT"
systemctl daemon-reload
systemctl enable cannabits-gateway >/dev/null 2>&1 || true

cat <<EOF

Installed. Two edits, then start:

  1. sudo nano $ETC/gateway.env    # CANNABITS_API_KEY=cbk_...  (URL is already the public backend)
  2. sudo nano $ETC/gateway.json   # planId = your plan UUID, gatewayLabel = a name
  3. sudo systemctl start cannabits-gateway
  4. journalctl -u cannabits-gateway -f     # each flush logs "sent ...: N stored"

The OptiClimate is connected to a room in the Cannabits Pro UI (host + port);
the gateway picks it up automatically on the next config poll.
EOF
