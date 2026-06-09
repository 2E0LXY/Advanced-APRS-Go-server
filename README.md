# Advanced APRS Go Server

A high-performance, bidirectional APRS-IS gateway written in Go. Provides a real-time tactical map dashboard, full APRS-IS client server on port 14580, WebSocket API, and a complete browser-based admin interface.

Live instance: **[www.aprsnet.uk](https://www.aprsnet.uk)**

[![Release](https://img.shields.io/github/v/release/2E0LXY/Advanced-APRS-Go-server)](https://github.com/2E0LXY/Advanced-APRS-Go-server/releases)
[![Licence: GPL v3](https://img.shields.io/badge/Licence-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)

---

## Client Applications

The server powers four dedicated clients — all open source under GPL v3:

| Client | Platform | Repository | Download |
|--------|----------|------------|----------|
| **APRS Net Android** | Android 8+ | [2E0LXY/APRS-Android](https://github.com/2E0LXY/APRS-Android) | [APK / AAB](https://github.com/2E0LXY/APRS-Android/releases) |
| **APRS Net iOS** | iOS 17+ | [2E0LXY/APRS-iOS](https://github.com/2E0LXY/APRS-iOS) | [Simulator build](https://github.com/2E0LXY/APRS-iOS/releases) |
| **APRS Client** (Windows/Linux) | Windows 10/11, Debian/Ubuntu | [2E0LXY/APRS-Client](https://github.com/2E0LXY/APRS-Client) | [EXE / DEB](https://github.com/2E0LXY/APRS-Client/releases) |
| **Web dashboard** | Any browser | this repo | [www.aprsnet.uk](https://www.aprsnet.uk) |

**Android** — native Kotlin/Jetpack Compose; live map, messaging, smart beaconing, direct aisstream.io AIS, TOCALL-based station classification, member account sync.

**iOS** — native SwiftUI; live MapKit map, messaging, GPS beaconing, direct aisstream.io AIS, identical TOCALL classification logic.

**Windows/Linux desktop** — Electron wrapper; system tray, native OS notifications, GPS map marker, auto member login, two-way filter persistence, launch on startup.

---

## Features

### 🗺 Real-time Map Dashboard
- Live station map with OpenStreetMap, Terrain and Satellite tile layers
- 40+ APRS symbol types rendered with correct glyphs per packet symbol table
- Position trails, PHG transmitter range circles, solar day/night terminator
- **TOCALL-based station classification** — firmware-accurate detection of LoRa (`APLRG*`, `APLRT*`, `APLG*`), MMDVM/DMR (`APZDMR*`, `APDG*`), and OGN receivers (`APOG*`) in addition to callsign-string heuristics
- Station type filters: Ham APRS, CWOP Weather, OGN Gliders, LoRa, MMDVM/DMR, Ships/AIS, Objects
- Click any callsign or map marker to open QRZ.com profile

### 📡 APRS-IS Gateway
- Connects to any APRS-IS Tier 2 server as a verified client
- Full APRS-IS login/logresp handshake with callsign verification
- Configurable subscription filter with visual builder
- Q-construct injection, duplicate suppression, automatic reconnection

### 📱 TCP APRS-IS Client Server (port 14580)
- Standard APRS-IS protocol — compatible with APRSDroid, YAAC, APRSIS32, Xastir, Direwolf
- Full login/logresp handshake; verified clients can inject packets upstream
- Keepalive lines every 20 seconds

### 🛰 UDP Hardware Tracker Support (port 14580)
- Stateless UDP listener for hardware trackers and IoT devices
- No login required — packets injected with qAU construct

### 💬 APRS Messaging
- Station-to-station messaging via WebSocket authenticated clients
- Automatic ACK generation per APRS spec
- Offline message delivery to member accounts
- Desktop browser notifications, unread count badge, quiet hours

### 🌊 Live AIS Ships
- Server subscribes to [aisstream.io](https://aisstream.io) and relays marine vessel positions to all connected clients (web, Android, iOS, desktop)
- Coverage: UK + NW European coastal waters (48–62 N, 12 W–5 E)
- API key configured in Admin panel (`ais_stream_key`)
- Android and iOS apps also support a **direct** aisstream.io connection (separate API key) for independent vessel feeds

### 📊 Propagation & Network Analytics
- Activity meter, coverage meter, high-activity alerts
- Propagation lines, station reliability grades, longest RF paths
- 7-day activity heatmap, best time of day histogram

### 🌦 Weather Overlays
- Animated RainViewer rain-radar overlay (no key needed)
- UK Met Office severe weather warnings, colour-coded yellow/amber/red

### 🔑 Third-Party Integrations
- **QRZ.com XML** — operator profiles in the station detail modal
- **aisstream.io** — live AIS ship positions (see above)
- **Met Office DataHub** — reserved for future detailed-forecast features

### ⚙️ Admin Panel
- First-run setup wizard at `/setup`
- Hot-reload config, one-click update from GitHub, change password
- Filter builder for all 8 APRS-IS filter types
- Server-wide drop filters for Pi-Star, D-STAR, APDESK traffic

---

## Installation (Debian 12)

### One-line Deploy
```bash
apt update && apt install -y git && \
git clone https://github.com/2E0LXY/Advanced-APRS-Go-server /opt/aprs-gateway && \
cd /opt/aprs-gateway && chmod +x install.sh && ./install.sh
```

On first visit navigate to your domain — you will be redirected to `/setup`.

### Firewall Ports
| Port | Protocol | Purpose |
|------|----------|---------|
| 80 | TCP | HTTP (redirected to HTTPS by Caddy) |
| 443 | TCP | HTTPS web interface |
| 14580 | TCP | APRS-IS client connections |
| 14580 | UDP | Hardware tracker UDP submit |

### Deploy One-liner
```bash
ssh root@yourserver 'cd /opt/aprs-gateway && git pull origin main && \
  /usr/local/go/bin/go build -o aprs_server . && systemctl restart aprs'
```

---

## Connecting Clients

### APRSDroid / Direwolf / YAAC / APRSIS32
Server: `your-domain.com`, Port: `14580`, SSL: **off**

### UDP Hardware Tracker
```bash
echo "M0XYZ>APRS,TCPIP*:=5342.10N/00130.50W-Test" | nc -u -w1 YOUR-IP 14580
```

---

## API Reference

### WebSocket — `wss://your-domain/ws`
```json
{ "type": "auth", "callsign": "M0XYZ", "passcode": "12345", "software": "MyApp 1.0" }
{ "type": "tx",   "packet":   "M0XYZ>APRS,TCPIP*:=5342.10N/00130.50W-Status"         }
```

### REST Endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `GET /api/status` | No | Uptime, packet counts, upstream status, connected clients |
| `GET /api/history` | API key | Last 10,000 decoded position packets |
| `GET /api/version` | No | Running server version |
| `GET /api/analytics` | No | Reliability grades, longest paths, heatmap |
| `GET /api/qrz/lookup?call=X` | No | QRZ.com operator profile (cached 24 h) |
| `GET /api/wx/warnings` | No | UK Met Office warnings (cached 5 min) |
| `GET /api/config` | Yes | Full server configuration JSON |
| `POST /api/config` | Yes | Update and hot-reload configuration |
| `GET /api/member/preferences` | X-Member-Token | Per-member map filter preferences |
| `PUT /api/member/preferences` | X-Member-Token | Replace per-member map filter preferences |

### Per-member Map Filter Preferences
Preferences sync automatically between the web map and Android/iOS clients (v2.5.0+). Recognised keys:

| Key | Effect |
|-----|--------|
| `drop_pistar` | Hide MMDVM/Pi-Star/DMRGateway beacons |
| `drop_dstar` | Hide D-STAR gateway beacons |
| `drop_apdesk` | Hide APDESK (UI-View desktop) beacons |

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
- Unverified APRS-IS clients (passcode -1) are receive-only

---

## Licence
GNU General Public License v3.0 — see [LICENCE](LICENCE)

*Advanced APRS Go Server — 2E0LXY*
