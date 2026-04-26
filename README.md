# Advanced APRS Go Server
A high-performance, bidirectional **APRS-IS Gateway** engineered for stability, security, and low latency. This project replaces resource-heavy legacy implementations with a modern Go-based stack, providing a real-time visual NOC (Network Operations Centre) for Amateur Radio telemetry.
## 🚀 Key Features
 * **Bidirectional APRS-IS Routing**: Connects to global Tier 2 hubs via TCP with strict loop prevention.
 * **Modern Web Interface**: High-contrast, neon-themed tactical map using Leaflet.js and Tailwind CSS.
 * **Intelligent Filtering**: Built-in "Adjunct" engine to strip bloated telemetry (Pi-Star, D-STAR, APDESK) and server-side geofencing.
 * **Real-time Messaging**: Full station-to-station messaging with automatic protocol acknowledgements (ACK).
 * **Integrated API**: Secure WebSocket (WSS) and JSON endpoints for third-party developers and live metrics.
 * **Hardware Support**: Statutory UDP 14580 listener for stateless hardware trackers and IoT devices.
 * **Automatic SSL**: Powered by Caddy for zero-config HTTPS and encrypted administrative access.
## 🛠 Technical Stack
 * **Language**: Go (Golang) — Native concurrency with a tiny memory footprint.
 * **Web Server**: Caddy — Modern reverse proxy with auto-HTTPS.
 * **Frontend**: Vanilla JS — Framework-free for maximum browser performance.
 * **Security**: Token-bucket rate limiting and Q-construct injection (qAC/qAU).
## 📦 Installation (Debian 12)
This project includes an automated deployment script that handles Go installation, Caddy configuration, and systemd service setup.
### 1. Prerequisites
Ensure your domain A-record (e.g., theloxleys.uk) points to your VPS IP.
### 2. One-Line Deployment
Run the following command as **root** to clone and install the entire system:
```bash
apt update && apt install -y git && \
git clone https://github.com/2E0LXY/Advanced-APRS-Go-server /opt/aprs-gateway && \
cd /opt/aprs-gateway && chmod +x install.sh && ./install.sh

```
## 🔐 Administrative Access
The dashboard and configuration API are protected via HTTP Basic Auth.
 * **Default Username**: 2e0lxy
 * **Default Password**: 33wf31ug33
*Note: You can update these credentials directly in the basicAuth function within aprs_server.go.*
## 📑 API Endpoints
| Endpoint | Type | Description |
|---|---|---|
| / | HTTP | Main Tactical Map Dashboard |
| /ws | WSS | Bi-directional JSON Data Stream |
| /api/status | JSON | Real-time Packet & Client Metrics |
| /api/history | JSON | Recent Station Position History |
| /api/config | JSON | Protected Server Configuration |
## 🛡️ Firewall Configuration
The install.sh script configures ufw automatically. Ensure the following ports are permitted:
 * **80/443 (TCP)**: Web Traffic & SSL.
 * **14580 (UDP)**: Incoming Hardware Telemetry.
 * **14580 (TCP)**: Standard APRS-IS Uplink/Downlink.
## ⚖️ License
This project is intended for the Amateur Radio community. Use it to build faster, cleaner infrastructure. Stability and backend logic are prioritised over fashionable bloat.
