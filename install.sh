#!/bin/bash

# Advanced APRS Gateway Auto-Deploy
# Target: https://theloxleys.uk

set -e

APP_DIR="/opt/aprs-gateway"
REPO_URL="https://github.com/2E0LXY/Advanced-APRS-Go-server"
GO_VERSION="1.22.2"

if [ "$EUID" -ne 0 ]; then 
  echo "Please run as root."
  exit 1
fi

# Wipe corrupted Caddy lists from previous failed attempts
rm -f /etc/apt/sources.list.d/caddy-stable.list

apt update
apt install -y curl wget git ufw debian-keyring debian-archive-keyring apt-transport-https

if ! command -v go &> /dev/null; then
    wget "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
    rm -rf /usr/local/go && tar -C /usr/local -xzf "go$GO_VERSION.linux-amd64.tar.gz"
    export PATH=$PATH:/usr/local/go/bin
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /root/.profile
    rm "go$GO_VERSION.linux-amd64.tar.gz"
fi

if ! command -v caddy &> /dev/null; then
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    echo "deb [signed-by=/usr/share/keyrings/caddy-stable-archive-keyring.gpg] https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version main" > /etc/apt/sources.list.d/caddy-stable.list
    apt update && apt install caddy -y
fi

mkdir -p $APP_DIR
cd $APP_DIR

if [ ! -d ".git" ]; then
    git clone $REPO_URL .
else
    git pull origin main
fi

/usr/local/go/bin/go mod init aprs || true
/usr/local/go/bin/go get github.com/gorilla/websocket
/usr/local/go/bin/go build -o aprs_server aprs_server.go

cat <<EOF > /etc/systemd/system/aprs.service
[Unit]
Description=Advanced APRS Gateway
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/aprs_server
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable aprs
systemctl restart aprs

cp Caddyfile /etc/caddy/Caddyfile
systemctl restart caddy

ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 14580/udp
ufw allow 14580/tcp
ufw --force enable
