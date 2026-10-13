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
# Grow Goddess Pro: facility settings, Gateways, Add gateway -> a one-time code
sudo cannabits-gateway enroll EA3D-7NBB-3H0W-22JE-TXQ8-1WMW-HP0B   # this gateway's own key and the plan id
sudo nano /etc/cannabits/gateway.json          # gatewayLabel, and the options below
sudo systemctl restart cannabits-gateway
journalctl -u cannabits-gateway -f
```

The code (140 bits) works once and for 15 minutes. `enroll` keeps the key in
`/var/lib/cannabits-gateway/gateway.key` (0600, the service user's), where it
wins over `CANNABITS_API_KEY`. Once a gateway of a facility has enrolled, the
facility's plan keys stop working, so a Pi still running on a plan key must
enroll too.

**Rotation** is the gateway's own: `Rotate` in the app only flags it, and on
its next config poll the gateway asks for a new key with its current one,
stores it in `gateway.key`, then confirms with the new key. Both keys work
until the confirmation (at most 24 hours), so a failed step never locks the
Pi out. A new enrollment code (a replaced Pi or SD card) also replaces the
key. `Revoke` ends the gateway for good.

**Unauthorised.** When the platform answers 401 or 403 (revoked, replaced, a
retired plan key) the gateway keeps reading and spooling, logs
`UNAUTHORISED ...` at once and then every hour, and never drops a batch
because of it: the spool bound rises to ten times the normal one (about 40
days), and new readings stop being recorded only past that, or earlier when
the disk would keep less than `spoolMinFreePercent` free (default 10 %), each
time with a loud `UNAUTHORISED AND SPOOL FULL` line naming the reason. The first accepted call ends the
state and the spool drains.

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

A plan key minted in the app works there for a facility whose gateways have
not enrolled yet; `enroll` stores the gateway's own key elsewhere (above).

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
by its program), lights on/off and the light cell's level, the equipment
(cooling on/off and percentage, heating, de-humidify, fan percentage), and
CO₂ with its setpoint and dosing state once a CO₂ sensor answers and the CO₂
function is enabled on the box. The controller's alarms are reported as
chart events, once when raised and once when cleared. Every controller call opens its own connection and closes
it; the boxes drop idle Wi-Fi connections silently.

### Control (setpoint writes)

Since 2026-09-10 the gateway can also WRITE the four room setpoints
(temperature and humidity, day and night) that an operator changes in
Grow Goddess Pro. The platform queues a command; the gateway polls it (every
5 s, the same poll as Test Connection), checks the value against its own
guard band (see Security below) and the
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

## Security on the Pi (2026-10-14)

The gateway refuses these on its own, whatever the backend or a config asks:

- **https only.** The API URL must be `https://`; the gateway stops at
  startup otherwise. Plain `http://` works only for a backend on localhost,
  with `-insecure-localhost` (development).
- **Guard band.** A setpoint outside `guardBand` in `gateway.json` (default
  15 to 32 °C and 30 to 85 %RH) is refused before the controller is touched,
  even with `allowControl: true` and even when the platform asks.
- **Controller networks.** The gateway reaches a controller (a room binding
  from the config or a command) only at an IP inside `controllerNetworks`
  (default: the private and link-local ranges; every entry must be private)
  or at a box its own LAN scan found. Host names, loopback and public
  addresses are refused and logged as `agent: REFUSED controller ...`.
- **Its own user.** `install.sh` creates `cannabits-gateway`; the systemd
  unit runs as that user with `ProtectSystem=strict`, `ProtectHome`,
  `PrivateTmp`, no capabilities, write access only to
  `/var/lib/cannabits-gateway`, and only IPv4, IPv6, Unix and netlink
  sockets (netlink because the LAN scan reads the Pi's subnets from it).
  The spool and local readings are 0700 directories with 0600 files;
  `gateway.json` is 0600 for the service user, `gateway.env` 0600 for root.

### Sync policy: what leaves the facility

```json
{ "syncPolicy": "aggregated", "localRetentionDays": 7 }
```

| `syncPolicy` | Sent to the cloud | Kept on the Pi |
|---|---|---|
| `full` (default) | every reading, once a minute | the spool until confirmed |
| `aggregated` | one mean per sensor and hour, after the hour ends (`derivationMethod: hourly_mean`); on/off states are not sent | raw readings, `localRetentionDays` (default 7) |
| `local_only` | only controller alarms and link status | raw readings, `localRetentionDays` |

Alarms and link status go out under every policy. The owner can also set a
policy per gateway in the app; the gateway applies the stricter of the two
(logged as `agent: sync policy ...`), and the platform refuses what the
policy forbids even if it arrives. Local readings live in
`/var/lib/cannabits-gateway/local/readings-YYYY-MM-DD.jsonl` (or
`localDir`); older days are deleted. A restart in the middle of an hour
loses that hour's samples taken before it (aggregated).

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
