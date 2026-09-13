# growgoddess-pi — Cannabits Edge Gateway

The small Go program that runs **on a Raspberry Pi inside the facility**. It
reads the local climate controller (OptiClimate/Revomax over HTTP, or a passive
RS485 Modbus tap, or an I²C sensor), spools every batch to disk, and streams it
**out** to the public Cannabits backend over HTTPS. It measures; it writes a
setpoint only when the operator asks in Grow Goddess Pro AND this gateway is
configured with `allowControl: true` (see Control below).

```
OptiClimate / device  ──LAN──▶  Raspberry Pi  ──HTTPS──▶  Cannabits backend  ──▶  pro_readings  ──▶  Cannabits Pro (from anywhere)
```

The Pi is the **only** thing that ever touches the LAN device. Your laptop,
your phone, the browser and the cloud backend never connect to `192.168.x.x` or
the device — they read the stored telemetry from the backend. So once the Pi is
installed it keeps working whether or not you are on site.

> This repo is extracted from the Cannabits backend's `cmd/gateway` +
> `internal/gateway`. The code is identical; only the Go module path differs.

## Guarantees (all covered by `go test ./...`)

- **Nothing is lost.** A batch is written to the spool before the first send and
  deleted only after the backend confirms it. Outages, restarts, crashes: the
  spool drains in order on recovery, and fixed batch ids make every retry
  idempotent. The spool bound (default ~4 days) drops the oldest with a loud log
  line, never silently.
- **Honest by construction.** A gateway is either a live instrument or a labeled
  fixture; one `sim` source makes every batch `dataMode: "fixture"`.
- **Survives the internet dropping.** 30s HTTP timeout, bounded retry/backoff,
  reading continues while uploads fail, auto-drains when the link returns.

## Quick start

On the Pi (64-bit Raspberry Pi OS), with Go installed:

```bash
git clone https://github.com/neda-dvb/growgoddess-pi.git
cd growgoddess-pi
sudo ./install.sh
sudo nano /etc/cannabits/gateway.env    # CANNABITS_API_KEY=cbk_...
sudo nano /etc/cannabits/gateway.json   # planId + gatewayLabel
sudo systemctl start cannabits-gateway
journalctl -u cannabits-gateway -f
```

No Go on the Pi? Cross-compile on your Mac and copy the folder over:

```bash
./build.sh            # writes dist/cannabits-gateway (arm64; use `./build.sh arm` for 32-bit)
rsync -a . pi@<pi-host>:growgoddess-pi/
ssh pi@<pi-host> 'cd growgoddess-pi && sudo ./install.sh'
```

## Configuration

Secrets and the API host come from the environment
(`/etc/cannabits/gateway.env`) so they never sit in a committed file:

```
CANNABITS_API_URL=https://growgoddess.club
CANNABITS_API_KEY=cbk_...
```

Everything else is `/etc/cannabits/gateway.json`. **Agent mode** (the default in
the systemd unit) keeps it tiny — the Pi pulls each room's controller from the
backend, so you connect the OptiClimate once in the Cannabits Pro UI (its host
and port) and never touch the Pi again:

```json
{
  "planId": "<plan uuid>",
  "gatewayLabel": "cannaru-pi",
  "spoolDir": "/var/lib/cannabits-gateway/spool",
  "configPollSeconds": 15
}
```

The OptiClimate register map is built into the adapter, so agent mode needs no
register configuration. Per room it streams, once a minute: air temperature,
humidity, the setpoint pair the controller is actually holding (day or night,
by its program), lights on/off and the light cell's level, and CO₂ with its
setpoint and dosing state once a CO₂ sensor answers and the CO₂ function is
enabled on the box. Every controller call opens its own connection and closes
it; the boxes drop idle Wi-Fi connections silently.

### Control (setpoint writes)

Since 2026-09-10 the gateway can also WRITE the four room setpoints
(temperature and humidity, day and night) that an operator changes in
Grow Goddess Pro. The platform queues a command; the gateway polls it (every
5 s, the same poll as Test Connection), checks the value against the
controller's own alarm limits, writes ONE register with
`POST /backend/setRegisterValues`, reads it back, and reports the outcome.
Nothing is written unless `gateway.json` says so:

```json
{ "allowControl": true }
```

With the flag off (the default) every write command is refused with
"control is disabled on this gateway" and logged. Every performed write is
logged as `CONTROL: <host> zone <room> <setpoint> -> <register> <value> (was
<previous>, read back <value>)`. Commands the platform queued more than two
minutes ago are never applied.

<details>
<summary>Static mode (explicit sources, no backend config poll)</summary>

Drop `-agent` from the systemd `ExecStart` and list sources yourself. Requires
`zone` (a cycle id of the plan) and at least one source:

```json
{
  "planId": "<plan uuid>",
  "zone": "room-1",
  "gatewayLabel": "cannaru-pi",
  "flushSeconds": 60,
  "spoolDir": "/var/lib/cannabits-gateway/spool",
  "sources": [
    { "type": "opticlimate", "url": "http://192.168.2.110:4001", "intervalSeconds": 60,
      "registers": [ { "name": "Room1Temp", "metric": "air_temp" }, { "name": "Humidity", "metric": "rh" } ] }
  ]
}
```

Other source types: `sht31` (I²C), `modbus-listen` (passive RS485 tap;
`-modbus-inspect` maps the bus first), `sim` (fixture data for a dry run).
</details>

### Discovery and re-find

"Find on the network" in the Pro connect dialog makes this gateway scan its
own /24 for boxes answering the OptiClimate API (read-only, about 3 s) and
report each with its MAC and live readings. Bindings remember the MAC; when
a box stops answering for three polls the gateway rescans and, if the MAC
answers on another address, reports the move so the room follows it. Logged
as `agent: discovery scanned N hosts ...`, `agent: learned MAC ...` and
`agent: controller for zone X moved: A -> B`.

### The Pi itself

Since 2026-09-13 the Pi accepts SSH by key only (`/etc/ssh/sshd_config.d/50-cannabits.conf`)
and listens on port 22 alone (rpcbind is disabled). To reach it from a new
machine, add that machine's public key to `~neda/.ssh/authorized_keys` first,
from a machine that already has access.

## Verify from a different network (phone hotspot)

1. On the Pi: `journalctl -u cannabits-gateway -f` shows `sent ...: N stored`
   each flush.
2. Turn off Wi-Fi, use mobile data, open Cannabits Pro → facility Overview. The
   room's values update on the 30s poll. If the Pi goes quiet the room shows
   **Stale** but keeps the last reading (5-minute freshness threshold).
3. The Pi→cloud push works from anywhere with just the key (no LAN, no browser):

```bash
curl -sS -X POST https://growgoddess.club/api/pro/plans/<plan-id>/telemetry \
  -H "X-Api-Key: cbk_..." -H "Content-Type: application/json" \
  -d '{"schemaVersion":"1.1","batchId":"manual-'"$(date +%s)"'","sentAt":"'"$(date -u +%FT%TZ)"'","dataMode":"live","readings":[{"sensorId":"<zone-id>:air_temp","type":"air_temp","ts":"'"$(date -u +%FT%TZ)"'","value":24.1}]}'
```

`{"stored":1}` means the cloud accepted it; the Overview shows it on refresh.

## Development

```bash
go build ./...        # native build
go test ./...         # the guarantees above are tested
go run ./cmd/gateway -config gateway.json          # static, with a sim source
go run ./cmd/gateway -config gateway.json -agent   # agent mode
```

Adding a controller vendor means adding one file implementing the four-method
`Source` interface in `internal/gateway`; the core never changes.
