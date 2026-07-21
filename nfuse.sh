#!/bin/bash

set -e
if [ "$(id -u)" != "0" ]; then
    echo "Please run this command as root user："
    echo "sudo bash <(curl -fsSL https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse.sh)"
    exit 1
fi

echo "Detecting default network interface..."
IFACE=$(ip route | awk '/default/ {print $5; exit}')

if [ -z "$IFACE" ]; then
    echo "Error: Unable to detect the default network interface!"
    exit 1
fi

echo "Network interface detected：$IFACE"
echo "Downloading the latest version of Nfuse..."
wget -O nfuse-amd64.tar.gz https://github.com/sketchain/Nfuse/releases/latest/download/nfuse-amd64.tar.gz
echo "Decompressing objects..."
tar -zxf nfuse-amd64.tar.gz
echo "Installing Nfuse to system..."
mv nfuse /usr/local/bin/nfuse
chmod +x /usr/local/bin/nfuse

echo "Creating systemd config..."
cat >/etc/systemd/system/nfuse.service <<EOF
[Unit]
Description=Nfuse per-port traffic metering and circuit breaker
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nfuse server --iface ${IFACE}
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

echo "Reloading systemd..."
systemctl daemon-reload
echo "Starting Nfuse and making it launch automatically at system startup..."
systemctl enable --now nfuse

echo
echo "========================================="
echo "Nfuse installation complete!"
echo "Interface：${IFACE}"
echo
echo "To check the program running status:"
echo "systemctl status nfuse"
echo
echo "To check the logs"
echo "journalctl -u nfuse -f"
echo "========================================="
