#!/bin/zsh
set -eu
UID_NUM=$(id -u)
echo "正在卸载 DJOneHub..."

# The mobile-network wake helper lives inside the attached module. Ask the
# authenticated local backend to remove it before stopping the LaunchAgent.
TOKEN_FILE="$HOME/Library/Application Support/DJOneHub/api-token"
if [ -r "$TOKEN_FILE" ]; then
  TOKEN=$(tr -d '\r\n' <"$TOKEN_FILE")
  if ! curl -fsS --max-time 25 \
    -H "X-DJOneHub-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{}' \
    http://127.0.0.1:7575/api/module/network-wake/uninstall >/dev/null 2>&1; then
    echo "提示：模块未连接或清理失败；Mac 端仍会继续卸载。下次连接模块后，可先切回 Mac 模式再重新运行卸载程序。"
  fi
fi
launchctl bootout "gui/$UID_NUM/com.jamie.djonehub" >/dev/null 2>&1 || true
launchctl bootout "gui/$UID_NUM/com.jamie.djonehub-notifier" >/dev/null 2>&1 || true
rm -f "$HOME/Library/LaunchAgents/com.jamie.djonehub.plist" "$HOME/Library/LaunchAgents/com.jamie.djonehub-notifier.plist"
rm -rf "$HOME/Library/Application Support/DJOneHub"
echo "已卸载。"
sleep 1
