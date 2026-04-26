## Project Overview: High-Performance APRS Gateway
This project is a modern, high-performance **APRS-IS Gateway** designed for stability and efficiency. It replaces resource-heavy legacy implementations with a streamlined stack that runs on minimal hardware. It functions as a central hub for Amateur Radio telemetry, providing a real-time visual interface while maintaining strict routing standards for the global APRS network.
### Core Technical Stack
 * **Language:** **Go (Golang)**. Leverages native concurrency and a tiny memory footprint, avoiding the bloat of virtual machine runtimes.
 * **Web Server:** **Caddy**. Acts as a high-speed reverse proxy with automatic **HTTPS (SSL)** management.
 * **Frontend:** **Vanilla JavaScript & Tailwind CSS**. Uses **Leaflet.js** for mapping to ensure the browser remains responsive even with hundreds of live markers.
 * **Data Transport:** **WebSockets (WSS)** for the web UI and **TCP/UDP** for upstream network links and hardware trackers.
### Available Features
| Feature Category | Capabilities |
|---|---|
| **Bidirectional Routing** | Links to global master servers via TCP. Supports full data flow with standard passcode authentication. |
| **Advanced Filtering** | Integrated engine to drop unwanted telemetry (Pi-Star, D-STAR, etc.) and a server-side geofence to filter by geographic radius. |
| **Tactical Map UI** | Real-time tracking with Maidenhead Grids, Day/Night overlays, PHG coverage circles, and movement trails. |
| **Integrated Messaging** | Full station-to-station messaging with automatic protocol acknowledgements (ACK). |
| **System Metrics** | Real-time dashboard tracking uptime, packet counts, throughput, and active client lists. |
| **Network Integrity** | Rate limiting (1 pkt/sec) and Q-Construct injection (qAC/qAU) to prevent routing loops and network abuse. |
| **Hardware Support** | Listener for blind-fire UDP packets from hardware trackers and IoT devices. |
### Technical Implementation
 * **Behavioral Interfaces:** We used Go interfaces to handle different connection types (WebSocket, UDP, TCP) through a single routing logic, ensuring code remains lean.
 * **In-Memory Ring Buffer:** Historical data is managed in a fast circular buffer, allowing users to sync recent history without the overhead of an external database.
 * **Local Validation:** The server calculates APRS-IS passcode hashes locally to verify users before passing data upstream, protecting the node's reputation.
 * **Path Scrubbing:** Strict duplicate checking hashes packet payloads and cleans TNC-2 headers to ensure network efficiency.
### Installation Instructions
This guide is for a fresh **Debian 12** installation with a domain name pointed to the server IP.
#### 1. System Preparation
Login as root and update the system:
```bash
apt update && apt upgrade -y
apt install -y curl wget git ufw debian-keyring debian-archive-keyring apt-transport-https

```
#### 2. Install the Go Compiler
```bash
wget https://go.dev/dl/go1.22.2.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go1.22.2.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> /root/.profile

```
#### 3. Deploy from Repository
Clone your project files into the application directory:
```bash
mkdir -p /opt/aprs-gateway
cd /opt/aprs-gateway
# Clone your specific repository here
git clone <your-repo-url> . 
chmod +x install.sh
./install.sh

```
#### 4. Administrative Credentials
The gateway is configured with the following default credentials for the Admin panel:
 * **Username:** admin
 * **Password:** adminpass
#### 5. Port Configuration
Ensure the following ports are open on your firewall:
 * **80/443 (TCP):** Web interface and SSL.
 * **14580 (UDP):** Incoming hardware telemetry.
 * **14580 (TCP):** Internal routing (proxied by Caddy).
### Final Summary
The result is a professional-grade APRS node that adheres to modern network standards. It provides a secure, encrypted frontend for users while maintaining the raw high-speed connections required for global radio telemetry. It is engineered to be accurate, responsive, and extremely stable under heavy load.