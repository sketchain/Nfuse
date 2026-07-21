#!/bin/bash

set -e
if [ "$(id -u)" != "0" ]; then
    echo "请使用 root 运行："
    echo "sudo bash <(curl -fsSL https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse_cn.sh)"
    exit 1
fi

echo "正在检测默认网卡..."
IFACE=$(ip route | awk '/default/ {print $5; exit}')

if [ -z "$IFACE" ]; then
    echo "错误：无法检测到默认网卡。"
    exit 1
fi

echo "检测到网卡：$IFACE"
echo "正在下载最新版 Nfuse..."
wget -O nfuse-amd64.tar.gz https://github.com/sketchain/Nfuse/releases/latest/download/nfuse-amd64.tar.gz
echo "解压中..."
tar -zxf nfuse-amd64.tar.gz
echo "安装 Nfuse 中..."
mv nfuse /usr/local/bin/nfuse
chmod +x /usr/local/bin/nfuse

echo "创建 systemd 服务..."
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

echo "重新加载 systemd 中..."
systemctl daemon-reload
echo "正在设置 Nfuse 开机启动并立即启动..."
systemctl enable --now nfuse

echo
echo "========================================="
echo "Nfuse 安装完成！"
echo "网卡：${IFACE}"
echo
echo "查看状态："
echo "systemctl status nfuse"
echo
echo "查看日志："
echo "journalctl -u nfuse -f"
echo "========================================="
