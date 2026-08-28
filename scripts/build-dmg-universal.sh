#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v1.2.9}
DMG_NAME="DJOneHub-macOS-universal-${VERSION}.dmg"
STAGE="${ROOT_DIR}/dist/dmg-stage-universal"
DMG="${ROOT_DIR}/dist/${DMG_NAME}"
CHECKSUM="${DMG}.sha256"
NOTIFIER_SRC="${ROOT_DIR}/macos/DJOneHubNotifier"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-macos-package-universal"

echo "=========================================="
echo "  构建标准 macOS DMG 安装包 (${VERSION})"
echo "=========================================="

echo "==> 1/3 构建 App Bundle（Go 后端 + Swift 主程序）"

# 编译通用 Go 后端
"${ROOT_DIR}/scripts/package-macos-universal.sh" "${VERSION}"

# 编译 DJOneHubNotifier Universal（方案 A：DJOneHubNotifier 即为主程序）
mkdir -p "${BUILD_ROOT}/local-cache/clang" "${BUILD_ROOT}/local-cache/swiftpm"
export CLANG_MODULE_CACHE_PATH="${BUILD_ROOT}/local-cache/clang"
export SWIFTPM_MODULECACHE_OVERRIDE="${BUILD_ROOT}/local-cache/clang"
export SWIFTPM_CUSTOM_CACHE_PATH="${BUILD_ROOT}/local-cache/swiftpm"
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
if [ ! -x "${DEVELOPER_DIR}/Toolchains/XcodeDefault.xctoolchain/usr/bin/swiftc" ]; then
  echo "A full Xcode installation is required to build the Intel notifier slice." >&2
  exit 1
fi
cd "${NOTIFIER_SRC}"
swift build --disable-sandbox -c release
"${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" --self-test

# SwiftPM 在 Apple Silicon 主机上默认只生成 native slice，显式构建 Intel slice
# 包含两个 C 目标（CModemBridge、CUACProbe），避免将 arm64-only 二进制伪称 Universal
INTEL_ROOT="${BUILD_ROOT}/notifier-x86_64"
rm -rf "${INTEL_ROOT}"
mkdir -p "${INTEL_ROOT}/module-cache"
cat > "${INTEL_ROOT}/CModemBridge.modulemap" <<EOF
module CModemBridge { header "${NOTIFIER_SRC}/Sources/CModemBridge/include/CModemBridge.h" export * }
EOF
cat > "${INTEL_ROOT}/CUACProbe.modulemap" <<EOF
module CUACProbe { header "${NOTIFIER_SRC}/Sources/CUACProbe/include/CUACProbe.h" export * }
EOF
xcrun clang -target x86_64-apple-macosx13.0 -O2 -fmodules \
  -fmodules-cache-path="${INTEL_ROOT}/module-cache" \
  -fmodule-map-file="${INTEL_ROOT}/CModemBridge.modulemap" \
  -I Sources/CModemBridge/include -c Sources/CModemBridge/ModemBridge.c \
  -o "${INTEL_ROOT}/ModemBridge.o"
xcrun clang -target x86_64-apple-macosx13.0 -O2 -fmodules \
  -fmodules-cache-path="${INTEL_ROOT}/module-cache" \
  -fmodule-map-file="${INTEL_ROOT}/CUACProbe.modulemap" \
  -I Sources/CUACProbe/include -c Sources/CUACProbe/CUACProbe.c \
  -o "${INTEL_ROOT}/CUACProbe.o"
xcrun swiftc -O -target x86_64-apple-macosx13.0 -sdk "$(xcrun --show-sdk-path)" \
  -Xcc -fmodules-cache-path="${INTEL_ROOT}/module-cache" \
  -Xcc -fmodule-map-file="${INTEL_ROOT}/CModemBridge.modulemap" \
  -Xcc -fmodule-map-file="${INTEL_ROOT}/CUACProbe.modulemap" \
  -I Sources/CModemBridge/include -I Sources/CUACProbe/include \
  Sources/DJOneHubNotifier/*.swift "${INTEL_ROOT}/ModemBridge.o" "${INTEL_ROOT}/CUACProbe.o" \
  -framework CoreAudio -framework CoreFoundation -framework IOKit -framework AVFoundation \
  -framework AppKit -framework UserNotifications -framework Contacts -framework ServiceManagement \
  -o "${INTEL_ROOT}/DJOneHubNotifier"
rm -f "${BUILD_ROOT}/DJOneHubNotifier-universal"
lipo -create "${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" "${INTEL_ROOT}/DJOneHubNotifier" \
  -output "${BUILD_ROOT}/DJOneHubNotifier-universal"
file "${BUILD_ROOT}/DJOneHubNotifier-universal" | cut -c1-120
for arch in arm64 x86_64; do
  lipo "${BUILD_ROOT}/DJOneHubNotifier-universal" -verify_arch "${arch}"
done

echo "==> 2/3 组装 DJOneHub.app Bundle"
rm -rf "${STAGE}"
mkdir -p "${STAGE}"
"${ROOT_DIR}/scripts/create-app-bundle.sh" "${VERSION}" universal "${STAGE}/DJOneHub.app" "${BUILD_ROOT}/DJOneHubNotifier-universal"

# 校验双架构
for binary in \
  "${STAGE}/DJOneHub.app/Contents/MacOS/DJOneHub" \
  "${STAGE}/DJOneHub.app/Contents/Resources/djonehub-macos" \
  "${STAGE}/DJOneHub.app/Contents/Resources/lib/libusb-1.0.0.dylib"
do
  for arch in arm64 x86_64; do
    lipo "${binary}" -verify_arch "${arch}"
  done
done

# 公开 DMG 不能包含模块侧通话运行时
if find "${STAGE}" -type f \( -name '*.ko' -o -name '*.armv7' \) | grep -q .; then
  echo "Public DMG unexpectedly contains a module-side runtime." >&2
  exit 1
fi

echo "==> 3/3 创建 Applications 快捷方式并生成 DMG"
ln -s /Applications "${STAGE}/Applications"

rm -f "${DMG}" "${CHECKSUM}"
hdiutil create -volname "DJOneHub" -srcfolder "${STAGE}" -ov -format UDZO "${DMG}"
hdiutil verify "${DMG}"
(
  cd "$(dirname -- "${DMG}")"
  shasum -a 256 "$(basename -- "${DMG}")" >"$(basename -- "${CHECKSUM}")"
)

echo
echo "✅ DMG 创建完成: ${DMG}"
echo "校验：${CHECKSUM}"
