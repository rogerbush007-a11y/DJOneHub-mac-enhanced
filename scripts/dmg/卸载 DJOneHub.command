#!/bin/zsh
set -eu
UID_NUM=$(id -u)
echo "正在卸载 DJOneHub..."
launchctl bootout "gui/$UID_NUM/com.jamie.djonehub" >/dev/null 2>&1 || true
launchctl bootout "gui/$UID_NUM/com.jamie.djonehub-notifier" >/dev/null 2>&1 || true
rm -f "$HOME/Library/LaunchAgents/com.jamie.djonehub.plist" "$HOME/Library/LaunchAgents/com.jamie.djonehub-notifier.plist"
rm -rf "$HOME/Library/Application Support/DJOneHub"
rm -rf "$HOME/Library/Logs/DJOneHub"
echo "已卸载。"
sleep 1
