#!/bin/sh
# Build and install DJOneHub on Linux (Fedora / systemd / PipeWire).
# Usage: sudo linux/install.sh [desktop-user]
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TARGET_USER=${1:-${SUDO_USER:-}}

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 root 运行：sudo $0 [用户名]" >&2
  exit 1
fi
if [ -z "${TARGET_USER}" ]; then
  echo "请指定运行 DJOneHub 的桌面用户：sudo $0 <用户名>" >&2
  exit 1
fi

if [ ! -x "${ROOT_DIR}/dist/djonehub-linux" ] || [ "${DJONEHUB_REBUILD:-0}" = "1" ]; then
  command -v go >/dev/null 2>&1 || { echo "需要 Go：dnf install golang" >&2; exit 1; }
  (cd "${ROOT_DIR}" && CGO_ENABLED=0 go build -mod=mod -trimpath -buildvcs=false \
    -ldflags="-s -w" -o dist/djonehub-linux ./cmd/djonehub-macos)
fi

install -Dm755 "${ROOT_DIR}/dist/djonehub-linux" /usr/local/bin/djonehub-linux
install -Dm644 "${ROOT_DIR}/linux/70-djonehub.rules" /etc/udev/rules.d/70-djonehub.rules
install -Dm644 "${ROOT_DIR}/linux/djonehub.service" /etc/systemd/user/djonehub.service
install -Dm644 "${ROOT_DIR}/linux/djonehub.desktop" /usr/local/share/applications/djonehub.desktop
install -Dm644 "${ROOT_DIR}/linux/README-Linux.md" /usr/local/share/doc/djonehub/README-Linux.md

# Hand MI_01 / MI_03 over from ModemManager.
udevadm control --reload
udevadm trigger --action=change --subsystem-match=tty
udevadm settle
if systemctl is-active --quiet ModemManager; then
  systemctl restart ModemManager
fi

# Serial access for sessions without a seat (e.g. SSH); uaccess covers the desktop.
usermod -aG dialout "${TARGET_USER}"

systemctl --user -M "${TARGET_USER}@" daemon-reload
systemctl --user -M "${TARGET_USER}@" enable djonehub.service
systemctl --user -M "${TARGET_USER}@" restart djonehub.service

echo "DJOneHub 已安装。打开 http://127.0.0.1:7575 ，或在应用菜单中启动 DJOneHub。"
echo "日志：journalctl --user -u djonehub -f"
