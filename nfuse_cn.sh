#!/bin/bash
# Nfuse 安装 / 升级脚本（中国大陆镜像版，与 nfuse.sh 逻辑一致）。
#
#   curl -fsSL https://gitd.uh.ink/https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse_cn.sh | sudo bash
#
# 可选环境变量：
#   NFUSE_IFACE=ens5   指定要计量的网卡（默认取默认路由所在网卡）
#
# 重复运行即原地升级二进制并重启服务；已存在的 /etc/systemd/system/nfuse.service
# 会被保留（其中可能有你自定义的参数），除非显式设置了 NFUSE_IFACE。

# 全部逻辑在 main() 里、最后一行才调用：下载被截断时 bash 什么都不会执行。
set -euo pipefail

REPO="sketchain/Nfuse"
MIRROR="https://gitd.uh.ink/"   # 加在 github.com 链接前的加速镜像前缀
BIN=/usr/local/bin/nfuse
UNIT=/etc/systemd/system/nfuse.service
INSTALL_CMD="curl -fsSL ${MIRROR}https://github.com/${REPO}/raw/refs/heads/main/nfuse_cn.sh | sudo bash"

die() { echo "错误：$*" >&2; exit 1; }

detect_arch() {
    case "$(uname -m)" in
        x86_64 | amd64)          echo amd64 ;;
        aarch64 | arm64)         echo arm64 ;;
        armv7* | armv8l)         echo armv7 ;;
        i386 | i486 | i586 | i686) echo 386 ;;
        *) die "不支持的 CPU 架构：$(uname -m)（支持 amd64、arm64、armv7、386）" ;;
    esac
}

detect_iface() {
    local dev
    for fam in -4 -6; do
        dev=$(ip "$fam" route show default 2>/dev/null |
            awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}' || true)
        if [ -n "$dev" ]; then
            echo "$dev"
            return
        fi
    done
}

check_kernel() {
    local rel major minor
    rel=$(uname -r)
    major=${rel%%.*}
    minor=${rel#*.}
    minor=${minor%%[!0-9]*}
    major=${major:-0} minor=${minor:-0}
    if [ "$major" -lt 5 ] || { [ "$major" -eq 5 ] && [ "$minor" -lt 16 ]; }; then
        die "需要 Linux >= 5.16（nftables netdev egress 钩子），当前内核为 $rel"
    fi
}

install_nftables() {
    command -v nft >/dev/null 2>&1 && return
    echo "未找到 nftables（nft），正在安装..."
    # </dev/null：脚本通过管道交给 bash 时，读取 stdin 的包管理器会吞掉剩余脚本。
    if command -v apt-get >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get update -qq </dev/null
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nftables </dev/null
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y -q nftables </dev/null
    elif command -v yum >/dev/null 2>&1; then
        yum install -y -q nftables </dev/null
    elif command -v zypper >/dev/null 2>&1; then
        zypper --non-interactive install nftables </dev/null
    elif command -v pacman >/dev/null 2>&1; then
        pacman -Sy --noconfirm nftables </dev/null
    fi
    command -v nft >/dev/null 2>&1 || die "无法自动安装 nftables，请手动安装后重新运行"
}

fetch() { # fetch <url> <dest>
    if command -v curl >/dev/null 2>&1; then
        curl -fL --retry 3 --connect-timeout 15 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        die "需要 curl 或 wget 来下载 Nfuse"
    fi
}

main() {
    if [ "$(id -u)" != "0" ]; then
        echo "请使用 root 运行："
        echo "$INSTALL_CMD"
        exit 1
    fi
    [ "$(uname -s)" = "Linux" ] || die "Nfuse 仅支持 Linux"
    command -v systemctl >/dev/null 2>&1 || die "需要 systemd（未找到 systemctl）"
    command -v sha256sum >/dev/null 2>&1 || die "需要 sha256sum 来校验下载文件（请安装 coreutils）"

    check_kernel
    local arch iface base asset
    arch=$(detect_arch)

    iface=${NFUSE_IFACE:-}
    if [ -z "$iface" ]; then
        echo "正在检测默认网卡..."
        iface=$(detect_iface)
        [ -n "$iface" ] || die "无法检测到默认网卡，请设置 NFUSE_IFACE=<网卡名>（参见：ip -br link）"
    fi
    [ -e "/sys/class/net/$iface" ] || die "网卡 '$iface' 不存在（参见：ip -br link）"
    echo "网卡：$iface"
    echo "架构：$arch"

    install_nftables

    # 全局变量（非 local），main 返回后 EXIT trap 仍能看到它。
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    base="${MIRROR}https://github.com/${REPO}/releases/latest/download"
    asset="nfuse-${arch}.tar.gz"
    echo "正在下载最新版 Nfuse..."
    fetch "$base/$asset" "$tmp/$asset"
    # 校验失败即中止：脚本以 root 运行，且每个 Release 都发布 SHA256SUMS.txt，
    # 因此无法验证的二进制一律不安装。
    echo "正在校验 SHA256..."
    fetch "$base/SHA256SUMS.txt" "$tmp/SHA256SUMS.txt" ||
        die "无法下载 SHA256SUMS.txt，拒绝安装未经校验的二进制"
    (cd "$tmp" && grep " ${asset}\$" SHA256SUMS.txt | sha256sum -c --quiet -) ||
        die "$asset 校验失败，已中止"

    echo "正在安装 Nfuse 到 $BIN..."
    tar -xzf "$tmp/$asset" -C "$tmp" nfuse
    install -m 0755 "$tmp/nfuse" "$BIN"

    if [ -f "$UNIT" ] && [ -z "${NFUSE_IFACE:-}" ]; then
        echo "保留已有的 $UNIT（设置 NFUSE_IFACE 可重新生成）"
    else
        echo "创建 systemd 服务..."
        cat >"$UNIT" <<EOF
[Unit]
Description=Nfuse per-port traffic metering and circuit breaker
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=$BIN server --iface ${iface}
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
    fi

    echo "重新加载 systemd..."
    systemctl daemon-reload
    echo "设置 Nfuse 开机自启并（重新）启动..."
    systemctl enable nfuse >/dev/null
    # 用 restart 而非 `enable --now`：升级时旧二进制仍在运行。
    # 服务启动失败（或立即崩溃重启）时安装脚本以非零退出，避免自动化误判成功。
    if ! systemctl restart nfuse || ! { sleep 2; systemctl is-active --quiet nfuse; }; then
        echo >&2
        echo "错误：Nfuse 已安装，但服务启动失败。最近的日志：" >&2
        journalctl -u nfuse -n 20 --no-pager >&2 || true
        echo "排查：systemctl status nfuse / journalctl -u nfuse -f" >&2
        exit 1
    fi

    echo
    echo "========================================="
    echo "Nfuse 安装完成！（$("$BIN" version 2>/dev/null || echo nfuse)）"
    echo "网卡：${iface}"
    echo
    echo "查看状态："
    echo "systemctl status nfuse"
    echo
    echo "查看日志："
    echo "journalctl -u nfuse -f"
    echo "========================================="
}

main "$@"
