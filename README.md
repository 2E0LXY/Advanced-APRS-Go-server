# Advanced APRS Go Server

A high-performance, bidirectional APRS-IS Gateway written in Go. Provides a real-time tactical map dashboard, full APRS-IS client server on port 14580, WebSocket API, and a complete browser-based admin interface.

[![Release](https://img.shields.io/github/v/release/2E0LXY/Advanced-APRS-Go-server)](https://github.com/2E0LXY/Advanced-APRS-Go-server/releases)
[![Licence: GPL v3](https://img.shields.io/badge/Licence-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)

---

## Features

### 🗺 Real-time Map Dashboard
- Live station map using Leaflet.js with OpenStreetMap, Terrain and Satellite tile layers
- 40+ APRS symbol types rendered with correct glyphs per packet symbol table
- Position trails with configurable length (5 min to 6 hours)
- PHG transmitter range circles
- Real-time solar day/night terminator overlay
- Station type filters: Ham APRS, CWOP Weather, OGN Gliders
- Click any callsign or map marker to open QRZ.com profile
- Station count and RX packet counter in header

### 📡 APRS-IS Gateway
- Connects to any APRS-IS Tier 2 server as a verified client
- Full APRS-IS login/logresp handshake with callsign verification
- Configurable subscription filter with visual builder (see below)
- Q-construct injection (qAC for WebSocket clients, qAU for UDP)
- Duplicate packet suppression (5-minute rolling cache)
- Automatic reconnection with configurable upstream server
- Keepalive loop every 20 seconds

### 📱 TCP APRS-IS Client Server (port 14580)
- Standard APRS-IS protocol — compatible with APRSDroid, YAAC, APRSIS32, Xastir, Direwolf
- Full login/logresp handshake per connected client
- Verified clients can inject packets upstream; unverified (pass -1) receive-only
- Keepalive lines sent to all TCP clients every 20 seconds
- All upstream packets forwarded to connected clients in real time

### 🛰 UDP Hardware Tracker Support (port 14580)
- Stateless UDP listener for hardware trackers and IoT devices
- No login required — packets injected with qAU construct

### 💬 APRS Messaging
- Station-to-station messaging via WebSocket authenticated clients
- Automatic ACK generation per APRS spec
- Message counter with sequential IDs
- **Click-to-Reply** — click a received message's sender to auto-fill the reply
- **Offline message delivery** — messages to a member's callsign are stored and
  delivered automatically when any client (web, APRSDroid, radio) next logs in
- **Notifications** — login toast for unread messages, live toasts, desktop
  browser notifications, unread count badge
- **Quiet Hours** — per-member configurable window that silences notification
  toasts and beeps (badge still updates)

### 📊 Propagation & Network Analytics
- **Activity meter** — packets-per-minute over the last 10 minutes (header gauge)
- **Coverage meter** — geographic spread of active stations (header gauge)
- **High Activity alert** — banner when network traffic spikes
- **Propagation lines** — animated map lines from stations toward the gateway,
  colour-coded by distance (green/lime/orange/purple)
- **Analytics panel** (in Leaderboard) computed from packet history:
  - Station Reliability — A–F grade per station from packet consistency
  - Longest RF Paths — ranking by digipeater hop count
  - Best Time of Day — 24-hour traffic histogram
  - 7-Day Activity Heatmap — day × hour packet-count grid
- **Auto-Fit Zoom** — map auto-frames all visible stations
- **Station Ghosting** — stale stations fade with a pulsing ring

### 🌦 Weather Overlays
- **Weather radar** — animated RainViewer rain-radar overlay (no key needed)
- **UK severe weather warnings** — Met Office National Severe Weather Warning
  banner, colour-coded yellow/amber/red (no key needed)

### 🔑 Third-Party Integrations (configured in Admin panel)
- **QRZ.com XML** — operator profiles (photo, address, grid, licence class)
  shown in the station detail modal. Needs a QRZ XML subscription.
- **aisstream.io** - Live Ships (AIS marine vessels). Server-side goroutine (`runAISStream`) subscribes to UK + NW European coastal waters (48-62N, 12W-5E). AIS position reports broadcast as `rx` WebSocket messages to all clients. Android and web receive live ships automatically. API key set in Admin panel (`ais_stream_key`).
- **Met Office DataHub** — optional API key field, reserved for future
  detailed-forecast features (severe weather warnings work without it)

### 📱 Mobile Companion Site
- Touch-optimised page at `/mobile` with a bottom tab bar
- Map / Stations / Messages / Status views
- Shares the same live WebSocket feed as the desktop site

### ⚙️ Admin Panel
- **First-run setup wizard** at `/setup` — creates admin credentials on first boot
- Credentials stored in `creds.json` (mode 0600), never committed to git
- Server config persisted to `server_config.json` — survives restarts
- **Server Identity** — server name shown in APRS-IS network, software version string
- **APRS-IS Uplink** — callsign + passcode (auto-computed from callsign as you type), upstream server dropdown
- **Filter Builder** — visual builder for all 8 APRS-IS filter types with plain-English descriptions
- **Drop Filters** — server-side drop of Pi-Star, D-STAR and APDESK traffic
- **Geofence** — optional server-side geographic boundary; packets outside radius dropped
- **Hot Reload** — apply config changes live without service restart
- **Change Password** — update admin password at runtime
- **One-click Update** — install new releases from GitHub with live progress log

### 🔧 APRS-IS Filter Builder
All filter types from [aprs-is.net/javAPRSFilter.aspx](https://www.aprs-is.net/javAPRSFilter.aspx) supported:

| Filter | Description |
|--------|-------------|
| `r/lat/lon/dist` | Range — positions within dist km of a point (up to 9) |
| `m/dist` | My Range — centred on your own last known position |
| `f/call/dist` | Friend Range — centred on another station (up to 9) |
| `p/G/M0/2E` | Prefix — callsigns starting with these prefixes |
| `b/call*` | Budlist — exact callsigns, wildcard * supported |
| `t/pwm` | Type — p=pos o=obj i=item m=msg q=query s=status t=telem u=user w=wx n=NWS |
| `a/N/W/S/E` | Area — bounding box (up to 9) |
| `d/MB7UH*` | Digipeater — packets digipeated through specific stations |

Prefix any filter with `-` to exclude matching packets from a broader subscription.

### 📊 Status Dashboard
- Live uptime, RX/TX packet counts, byte totals, dropped packets
- TCP client count
- Upstream server address with verified/connecting status
- Connected clients table showing TCP and WebSocket clients, callsign, type, address, verified status

### 🔄 Auto-update
- Checks GitHub releases API on load and every hour
- Pulsing yellow update button in header when new release available
- One-click install from Admin panel: git pull + go build + service restart
- Live progress log streamed to browser; page reloads automatically after restart

---

## Desktop & Mobile Clients

As well as this web dashboard, APRS Net is available as native applications.
Both wrap the same dashboard and add native GPS, system notifications, saved
settings, single sign-on and auto-connect. All three projects are open source
under GPL v3.

| Client | Repository | Download |
|--------|-----------|----------|
| Windows desktop | [2E0LXY/APRS-Client](https://github.com/2E0LXY/APRS-Client) | [Releases (.exe)](https://github.com/2E0LXY/APRS-Client/releases) |
| Android | [2E0LXY/APRS-Android](https://github.com/2E0LXY/APRS-Android) | [Releases (.apk)](https://github.com/2E0LXY/APRS-Android/releases) |

The **Windows client** is an Electron app with a system-tray icon, native
desktop notifications, launch-on-startup, GPS map plotting and an in-app
message composer.

The **Android client** is a Capacitor app delivering the full dashboard with a
touch layout, native GPS via Android location services, push notifications and
persistent settings.

Both are dedicated to www.aprsnet.uk - nothing to configure beyond a callsign.

---

## Installation (Debian 12)

### Prerequisites
- Domain A record pointing to your VPS IP
- Root access

### One-line Deploy
```bash
apt update && apt install -y git && \
git clone https://github.com/2E0LXY/Advanced-APRS-Go-server /opt/aprs-gateway && \
cd /opt/aprs-gateway && chmod +x install.sh && ./install.sh
```

On first visit, navigate to your domain — you will be redirected to `/setup` to create admin credentials.

### Firewall Ports
| Port | Protocol | Purpose |
|------|----------|---------|
| 80 | TCP | HTTP (redirected to HTTPS by Caddy) |
| 443 | TCP | HTTPS web interface |
| 14580 | TCP | APRS-IS client connections |
| 14580 | UDP | Hardware tracker UDP submit |

---

## Connecting Clients

### APRSDroid (Android)
- Connection Protocol: `APRS-IS (TCP/IP)`
- Server: `your-domain.com`, Port: `14580`
- SSL/TLS: **OFF**
- Callsign + APRS-IS passcode

### Direwolf iGate
```
IGSERVER your-domain.com
IGPORT 14580
IGLOGIN CALLSIGN passcode
```

### YAAC / APRSIS32 / Xastir
Add network connection: `your-domain.com:14580`

### UDP Hardware Tracker
```bash
echo "M0XYZ>APRS,TCPIP*:=5342.10N/00130.50W-Test" | nc -u -w1 YOUR-IP 14580
```

---

## API Reference

### WebSocket — `wss://your-domain/ws`
```json
// Authenticate:
{"type":"auth","callsign":"M0XYZ","passcode":"12345","software":"MyApp 1.0"}

// Transmit (authenticated clients only, callsign must match):
{"type":"tx","packet":"M0XYZ>APRS,TCPIP*:=5342.10N/00130.50W-Status"}

// Receive (pushed to all clients):
{"type":"rx","packet":"...","data":{"call":"M0XYZ","lat":53.7,"lon":-1.5,"sym":"/-","ts":1714136400}}
```

### REST Endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `GET /api/status` | No | Uptime, packet counts, upstream status, connected clients, integration flags |
| `GET /api/history` | API key | Last 10,000 decoded position packets |
| `GET /api/version` | No | Running server version |
| `GET /api/analytics` | No | Propagation analytics: reliability grades, longest paths, best time of day, 7-day heatmap |
| `GET /api/qrz/lookup?call=X` | No | QRZ.com operator profile lookup (server-side proxy, cached 24h) |
| `GET /api/wx/warnings` | No | UK Met Office severe weather warnings (cached 5 min) |
| `GET /api/config` | Yes | Full server configuration JSON |
| `POST /api/config` | Yes | Update and hot-reload configuration |
| `POST /api/password` | Yes | Change admin password |
| `GET /api/whoami` | Yes | Verify credentials |
| `POST /api/update` | Yes | Run update (SSE progress stream) |
| `GET /api/member/preferences` | X-Member-Token | Per-member map filter preferences (JSON blob) |
| `PUT /api/member/preferences` | X-Member-Token | Replace per-member map filter preferences |

---

## Member map filter preferences

Each registered member account stores a free-form JSON `preferences`
blob that controls which categories of traffic appear on their map.
These preferences sync automatically between the web map at
`aprsnet.uk` and the Android client v2.5.0+. Toggle them in either
place and the change appears on the other on next login.

**Recognised keys (more can be added without schema migration):**

| Key | Hides packets matching |
|---|---|
| `drop_pistar` | `PISTAR`, `MMDVM`, `APDPRS`, `APDG` (DMRGateway), `APIRCD`, `IRCDDB` |
| `drop_dstar` | `D-STAR`, `APDSTR` (D-STAR repeater forwarding) |
| `drop_apdesk` | `APDESK` (UI-View desktop client status) |

**Three filter layers, in order:**

1. **Upstream subscription filter** (admin) - controls what packets
   the gateway pulls from APRS-IS. Default `auto` builds a wide
   range filter from the geofence centre.
2. **Admin Drop Filters** (server-wide moderation) - apply to all
   clients. Useful for bandwidth or to mute persistent spammers.
3. **Member map filter preferences** (per account) - each member's
   own choice of what to see, synced web<->Android.

Anonymous web users see all stations by default; the per-member
filters never apply to them.

---

## Stack

| Component | Purpose |
|-----------|---------|
| Go | Gateway server, TCP/UDP listeners, HTTP API |
| Caddy | Reverse proxy, automatic HTTPS via Let's Encrypt |
| Leaflet.js | Interactive map |
| Tailwind CSS | UI styling |
| gorilla/websocket | WebSocket transport |

---

## Security Notes
- `creds.json` and `server_config.json` are created at runtime with mode 0600 and are gitignored
- Admin endpoints protected by HTTP Basic Auth
- WebSocket TX requires valid APRS-IS passcode matching the callsign
- `POST /api/update` runs shell commands — ensure your admin password is strong
- Unverified APRS-IS clients (passcode -1) are receive-only

---

## Licence
GNU General Public License v3.0 — see [LICENCE](LICENCE)

You are free to use, modify and distribute this software under GPL v3. Any derivative works must be open-sourced under the same licence and retain attribution to the original author (2E0LXY).

*Advanced APRS Go Server — 2E0LXY*
