#!/usr/bin/env bash
# Cross-compile the gateway for a Raspberry Pi from your Mac/PC.
# Produces a single static binary in dist/ — no runtime, no packages on the Pi.
#
#   ./build.sh            # arm64 (Pi 3/4/5, 64-bit Raspberry Pi OS) — default
#   ./build.sh arm        # armv7 (older 32-bit Pi / 32-bit OS)
#
# Then copy the whole folder to the Pi and run ./install.sh there, or just:
#   scp dist/cannabits-gateway pi@<pi-host>:/usr/local/bin/
set -euo pipefail
cd "$(dirname "$0")"
ARCH="${1:-arm64}"
out=dist/cannabits-gateway
mkdir -p dist
if [[ "$ARCH" == "arm" ]]; then
  GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -o "$out" ./cmd/gateway
else
  GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$out" ./cmd/gateway
fi
echo "built $out ($ARCH):"
file "$out" 2>/dev/null || true
