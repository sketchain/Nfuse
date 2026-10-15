#!/bin/bash
# Nfuse installer / upgrader.
#
#   curl -fsSL https://github.com/sketchain/Nfuse/raw/refs/heads/main/nfuse.sh | sudo bash
#
# Optional environment overrides:
#   NFUSE_IFACE=ens5   meter this interface instead of the default-route one
#
# Re-running the script upgrades the binary in place and restarts the service;
# an existing /etc/systemd/system/nfuse.service is kept (it may carry your own
# flags) unless NFUSE_IFACE is given.

# Everything lives in main(), called on the last line, so a truncated download
# piped into bash runs nothing instead of half a script.
set -euo pipefail

REPO="sketchain/Nfuse"
MIRROR=""   # prefix prepended to github.com URLs (used by the CN script)
BIN=/usr/local/bin/nfuse
UNIT=/etc/systemd/system/nfuse.service
INSTALL_CMD="curl -fsSL https://github.com/${REPO}/raw/refs/heads/main/nfuse.sh | sudo bash"

die() { echo "Error: $*" >&2; exit 1; }

detect_arch() {
    case "$(uname -m)" in
        x86_64 | amd64)          echo amd64 ;;
        aarch64 | arm64)         echo arm64 ;;
        armv7* | armv8l)         echo armv7 ;;
        i386 | i486 | i586 | i686) echo 386 ;;
        *) die "unsupported CPU architecture: $(uname -m) (supported: amd64, arm64, armv7, 386)" ;;
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
        die "Linux >= 5.16 is required (nftables netdev egress hook); this kernel is $rel"
    fi
}

install_nftables() {
    command -v nft >/dev/null 2>&1 && return
    echo "nftables (nft) not found, installing it..."
    # </dev/null: when this script is piped into bash, a package manager that
    # reads stdin would otherwise swallow the rest of the script.
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
    command -v nft >/dev/null 2>&1 || die "could not install nftables; install it manually and re-run"
}

fetch() { # fetch <url> <dest>
    if command -v curl >/dev/null 2>&1; then
        curl -fL --retry 3 --connect-timeout 15 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$2" "$1"
    else
        die "need curl or wget to download Nfuse"
    fi
}

main() {
    if [ "$(id -u)" != "0" ]; then
        echo "Please run this command as root:"
        echo "$INSTALL_CMD"
        exit 1
    fi
    [ "$(uname -s)" = "Linux" ] || die "Nfuse only runs on Linux"
    command -v systemctl >/dev/null 2>&1 || die "systemd is required (systemctl not found)"
    command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required to verify the download (install coreutils)"

    check_kernel
    local arch iface base asset
    arch=$(detect_arch)

    iface=${NFUSE_IFACE:-}
    if [ -z "$iface" ]; then
        echo "Detecting default network interface..."
        iface=$(detect_iface)
        [ -n "$iface" ] || die "unable to detect the default network interface; set NFUSE_IFACE=<iface> (see: ip -br link)"
    fi
    [ -e "/sys/class/net/$iface" ] || die "network interface '$iface' does not exist (see: ip -br link)"
    echo "Network interface: $iface"
    echo "Architecture: $arch"

    install_nftables

    # Global (not local) so the EXIT trap can still see it after main returns.
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    base="${MIRROR}https://github.com/${REPO}/releases/latest/download"
    asset="nfuse-${arch}.tar.gz"
    echo "Downloading the latest version of Nfuse..."
    fetch "$base/$asset" "$tmp/$asset"
    # Fail closed: this runs as root, and every release publishes SHA256SUMS.txt,
    # so a binary that cannot be verified is never installed.
    echo "Verifying checksum..."
    fetch "$base/SHA256SUMS.txt" "$tmp/SHA256SUMS.txt" ||
        die "could not download SHA256SUMS.txt; refusing to install an unverified binary"
    (cd "$tmp" && grep " ${asset}\$" SHA256SUMS.txt | sha256sum -c --quiet -) ||
        die "checksum verification failed for $asset; aborting"

    echo "Installing Nfuse to $BIN..."
    tar -xzf "$tmp/$asset" -C "$tmp" nfuse
    install -m 0755 "$tmp/nfuse" "$BIN"

    if [ -f "$UNIT" ] && [ -z "${NFUSE_IFACE:-}" ]; then
        echo "Keeping existing $UNIT (set NFUSE_IFACE to regenerate it)"
    else
        echo "Creating systemd service..."
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

    echo "Reloading systemd..."
    systemctl daemon-reload
    echo "Enabling Nfuse at boot and (re)starting it..."
    systemctl enable nfuse >/dev/null
    # restart, not `enable --now`: on an upgrade the old binary is still running.
    # A service that fails to start (or crash-loops right away) fails the
    # installer with a non-zero exit, so automation never sees a false success.
    if ! systemctl restart nfuse || ! { sleep 2; systemctl is-active --quiet nfuse; }; then
        echo >&2
        echo "Error: Nfuse was installed but the service failed to start. Recent logs:" >&2
        journalctl -u nfuse -n 20 --no-pager >&2 || true
        echo "Check: systemctl status nfuse / journalctl -u nfuse -f" >&2
        exit 1
    fi

    echo
    echo "========================================="
    echo "Nfuse installation complete! ($("$BIN" version 2>/dev/null || echo nfuse))"
    echo "Interface: ${iface}"
    echo
    echo "To check the program running status:"
    echo "systemctl status nfuse"
    echo
    echo "To check the logs:"
    echo "journalctl -u nfuse -f"
    echo "========================================="
}

main "$@"
