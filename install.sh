#!/usr/bin/env bash
# Install the Cannabits Edge Gateway on a Raspberry Pi. Run ON THE PI as root:
#   sudo ./install.sh
#
# It installs the binary, a dedicated service user, a hardened systemd service
# and config templates, then leaves the service enabled but stopped so you can
# enroll the gateway before it starts. Safe to re-run: it also moves an
# install that ran as root over to the service user.
set -euo pipefail

BIN=/usr/local/bin/cannabits-gateway
ETC=/etc/cannabits
STATE=/var/lib/cannabits-gateway
SPOOL=$STATE/spool
UNIT=/etc/systemd/system/cannabits-gateway.service
SVC_USER=cannabits-gateway
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

echo "==> service user"
if ! id -u "$SVC_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$STATE" --shell /usr/sbin/nologin "$SVC_USER"
  echo "    created $SVC_USER"
fi
# sensors on I2C and RS485 need these groups where they exist
for g in i2c dialout gpio; do
  if getent group "$g" >/dev/null; then usermod -aG "$g" "$SVC_USER"; fi
done

echo "==> config"
install -d -m 0755 "$ETC"
if [[ ! -f "$ETC/gateway.json" ]]; then
  install -m 0600 -o "$SVC_USER" -g "$SVC_USER" "$here/gateway.example.json" "$ETC/gateway.json"
  echo "    wrote $ETC/gateway.json"
fi
# the service reads gateway.json itself; nobody else needs to
chown "$SVC_USER:$SVC_USER" "$ETC/gateway.json"
chmod 0600 "$ETC/gateway.json"
if [[ ! -f "$ETC/gateway.env" ]]; then
  install -m 0600 -o root -g root "$here/gateway.env.example" "$ETC/gateway.env"
  echo "    wrote $ETC/gateway.env"
fi
# systemd reads the env file as root; it holds the key
chown root:root "$ETC/gateway.env"
chmod 0600 "$ETC/gateway.env"

echo "==> state (spool, local readings)"
install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$STATE" "$SPOOL"
# an install that ran as root left root-owned batches the service could not delete
chown -R "$SVC_USER:$SVC_USER" "$STATE"
find "$STATE" -type d -exec chmod 0700 {} +
find "$STATE" -type f -exec chmod 0600 {} +

echo "==> systemd service"
install -m 0644 "$here/systemd/cannabits-gateway.service" "$UNIT"
systemctl daemon-reload
systemctl enable cannabits-gateway >/dev/null 2>&1 || true

cat <<EOT

Installed. Enroll, then start:

  1. In Grow Goddess Pro: facility settings, Gateways, Add gateway. Copy the code.
  2. sudo cannabits-gateway enroll <code>       # this gateway's own key ($STATE/gateway.key) and the plan id
  3. sudo systemctl restart cannabits-gateway
  4. journalctl -u cannabits-gateway -f         # each flush logs "sent ...: N stored"

Without a code you can still paste a plan key: CANNABITS_API_KEY=cbk_... in
$ETC/gateway.env, and planId in $ETC/gateway.json.

The OptiClimate is connected to a room in the Cannabits Pro UI (host + port);
the gateway picks it up automatically on the next config poll.
EOT
