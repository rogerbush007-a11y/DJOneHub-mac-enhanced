#!/bin/sh
# Usage: sudo linux/uninstall.sh [desktop-user]
set -eu
TARGET_USER=${1:-${SUDO_USER:-}}
if [ -n "${TARGET_USER}" ]; then
  systemctl --user -M "${TARGET_USER}@" disable --now djonehub.service 2>/dev/null || true
fi
rm -f /usr/local/bin/djonehub-linux /etc/udev/rules.d/70-djonehub.rules \
  /etc/systemd/user/djonehub.service /usr/local/share/applications/djonehub.desktop
rm -rf /usr/local/share/doc/djonehub
udevadm control --reload
udevadm trigger --action=change --subsystem-match=tty
systemctl is-active --quiet ModemManager && systemctl restart ModemManager || true
echo "DJOneHub 已卸载；ModemManager 重新接管全部模块端口。"
