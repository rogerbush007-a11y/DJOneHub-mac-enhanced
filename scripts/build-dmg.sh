#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v0.1.3-preview}
DMG_NAME="DJOneHub-macOS-arm64-${VERSION}.dmg"
STAGE="${ROOT_DIR}/dist/dmg-stage-arm64"
DMG="${ROOT_DIR}/dist/${DMG_NAME}"

echo "=========================================="
echo "  构建标准 macOS DMG 安装包 (arm64, ${VERSION})"
echo "=========================================="

echo "==> 1/3 构建 App Bundle"
rm -rf "${STAGE}"
mkdir -p "${STAGE}"
"${ROOT_DIR}/scripts/create-app-bundle.sh" "${VERSION}" arm64 "${STAGE}/DJOneHub.app"

echo "==> 2/3 创建 Applications 快捷方式"
ln -s /Applications "${STAGE}/Applications"

echo "==> 3/3 生成 DMG 安装盘"
rm -f "${DMG}"
hdiutil create -volname "DJOneHub" -srcfolder "${STAGE}" -ov -format UDZO "${DMG}"
hdiutil verify "${DMG}"

echo
echo "✅ DMG 创建完成: ${DMG}"
