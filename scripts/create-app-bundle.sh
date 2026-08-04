#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v0.1.3-preview}
ARCH_TYPE=${2:-universal}
OUT_APP="${3:-"${ROOT_DIR}/dist/DJOneHub.app"}"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-app-build"

echo "=========================================="
echo "  构建 DJOneHub.app (${ARCH_TYPE})"
echo "=========================================="

# 1. 构建二进制组件
if [ "${ARCH_TYPE}" = "arm64" ]; then
    "${ROOT_DIR}/scripts/package-macos-arm64.sh" "${VERSION}"
    RELEASE_DIR="${ROOT_DIR}/dist/release/DJOneHub-macOS-arm64-${VERSION}"
    
    # 通知助手
    (cd "${ROOT_DIR}/macos/DJOneHubNotifier" && ./build-app.sh)
    NOTIFIER_APP="${ROOT_DIR}/macos/DJOneHubNotifier/dist/DJOneHubNotifier.app"
else
    "${ROOT_DIR}/scripts/package-macos-universal.sh" "${VERSION}"
    RELEASE_DIR="${ROOT_DIR}/dist/release/DJOneHub-macOS-universal-${VERSION}"
    
    # 通知助手 Universal
    mkdir -p "${BUILD_ROOT}/cache" "${BUILD_ROOT}/swiftpm"
    export CLANG_MODULE_CACHE_PATH="${BUILD_ROOT}/cache"
    export SWIFTPM_MODULECACHE_OVERRIDE="${BUILD_ROOT}/cache"
    export SWIFTPM_CUSTOM_CACHE_PATH="${BUILD_ROOT}/swiftpm"
    
    (
        cd "${ROOT_DIR}/macos/DJOneHubNotifier"
        xcrun swift build --disable-sandbox -c release
        xcrun swiftc -O -target x86_64-apple-macosx13.0 Sources/DJOneHubNotifier/*.swift -o "${BUILD_ROOT}/notifier-x86_64"
        lipo -create "${ROOT_DIR}/macos/DJOneHubNotifier/.build/release/DJOneHubNotifier" "${BUILD_ROOT}/notifier-x86_64" \
            -output "${BUILD_ROOT}/DJOneHubNotifier-universal"
    )
    NOTIFIER_APP="${BUILD_ROOT}/DJOneHubNotifier.app"
    rm -rf "${NOTIFIER_APP}"
    mkdir -p "${NOTIFIER_APP}/Contents/MacOS" "${NOTIFIER_APP}/Contents/Resources"
    cp "${BUILD_ROOT}/DJOneHubNotifier-universal" "${NOTIFIER_APP}/Contents/MacOS/DJOneHubNotifier"
    cp "${ROOT_DIR}/macos/DJOneHubNotifier/Info.plist" "${NOTIFIER_APP}/Contents/Info.plist"
    chmod 755 "${NOTIFIER_APP}/Contents/MacOS/DJOneHubNotifier"
fi

# 启动器
LAUNCHER_BIN="${BUILD_ROOT}/DJOneHubLauncher"
"${ROOT_DIR}/scripts/build-launcher.sh" "${ARCH_TYPE}" "${LAUNCHER_BIN}"

# 2. 组装 .app Bundle
echo "==> 组装 .app Bundle: ${OUT_APP}"
rm -rf "${OUT_APP}"
CONTENTS="${OUT_APP}/Contents"
MACOS_DIR="${CONTENTS}/MacOS"
RESOURCES_DIR="${CONTENTS}/Resources"

mkdir -p "${MACOS_DIR}" "${RESOURCES_DIR}/lib"

# 复制 Info.plist & PkgInfo
cp "${ROOT_DIR}/packaging/Info.plist" "${CONTENTS}/Info.plist"
# 替换 Version
plutil -replace CFBundleShortVersionString -string "${VERSION}" "${CONTENTS}/Info.plist"
plutil -replace CFBundleVersion -string "${VERSION}" "${CONTENTS}/Info.plist"
printf "APPL????" > "${CONTENTS}/PkgInfo"

# 复制主启动器
cp "${LAUNCHER_BIN}" "${MACOS_DIR}/DJOneHub"

# 复制图标
cp "${ROOT_DIR}/resources/DJOneHub.icns" "${RESOURCES_DIR}/DJOneHub.icns"
cp "${ROOT_DIR}/resources/StatusIcon.png" "${RESOURCES_DIR}/StatusIcon.png"
cp "${ROOT_DIR}/resources/StatusIcon@2x.png" "${RESOURCES_DIR}/StatusIcon@2x.png"

# 复制 Go 后端 & libusb
cp "${RELEASE_DIR}/bin/djonehub-macos" "${RESOURCES_DIR}/djonehub-macos"
cp "${RELEASE_DIR}/lib/libusb-1.0.0.dylib" "${RESOURCES_DIR}/lib/libusb-1.0.0.dylib"
ln -sfn libusb-1.0.0.dylib "${RESOURCES_DIR}/lib/libusb-1.0.dylib"

# 复制通知助手
ditto --norsrc --noextattr --noqtn --noacl "${NOTIFIER_APP}" "${RESOURCES_DIR}/DJOneHubNotifier.app"

# 3. 权限与代码签名
chmod 755 "${MACOS_DIR}/DJOneHub" "${RESOURCES_DIR}/djonehub-macos" "${RESOURCES_DIR}/lib/libusb-1.0.0.dylib"
chmod 755 "${RESOURCES_DIR}/DJOneHubNotifier.app/Contents/MacOS/DJOneHubNotifier"

echo "==> 对 Bundle 进行代码签名"
codesign --force --deep --sign - "${OUT_APP}"
codesign --verify --deep --strict "${OUT_APP}"

echo "✅ App Bundle 构建完毕: ${OUT_APP}"
