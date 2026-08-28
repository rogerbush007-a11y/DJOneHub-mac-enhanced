#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v1.2.9}
ARCH_TYPE=${2:-universal}
OUT_APP="${3:-"${ROOT_DIR}/dist/DJOneHub.app"}"
# 可选：外部传入已编译好的 Universal 二进制（由 build-dmg-universal.sh 调用时使用）
PREBUILT_BINARY="${4:-}"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-app-build"
NOTIFIER_SRC="${ROOT_DIR}/macos/DJOneHubNotifier"

echo "=========================================="
echo "  构建 DJOneHub.app (${ARCH_TYPE})"
echo "  方案 A：DJOneHubNotifier 作为主程序"
echo "=========================================="

# 1. 构建 Go 后端
if [ "${ARCH_TYPE}" = "arm64" ]; then
    "${ROOT_DIR}/scripts/package-macos-arm64.sh" "${VERSION}"
    RELEASE_DIR="${ROOT_DIR}/dist/release/DJOneHub-macOS-arm64-${VERSION}"
else
    "${ROOT_DIR}/scripts/package-macos-universal.sh" "${VERSION}"
    RELEASE_DIR="${ROOT_DIR}/dist/release/DJOneHub-macOS-universal-${VERSION}"
fi

# 2. 确定 DJOneHubNotifier Universal 二进制路径
if [ -n "${PREBUILT_BINARY}" ] && [ -f "${PREBUILT_BINARY}" ]; then
    # 由 build-dmg-universal.sh 传入已编译好的 Universal 二进制
    NOTIFIER_BIN="${PREBUILT_BINARY}"
    echo "==> 使用外部传入的 Universal 二进制: ${NOTIFIER_BIN}"
elif [ "${ARCH_TYPE}" = "arm64" ]; then
    # 仅构建 arm64
    mkdir -p "${BUILD_ROOT}/local-cache/clang" "${BUILD_ROOT}/local-cache/swiftpm"
    export CLANG_MODULE_CACHE_PATH="${BUILD_ROOT}/local-cache/clang"
    export SWIFTPM_MODULECACHE_OVERRIDE="${BUILD_ROOT}/local-cache/clang"
    export SWIFTPM_CUSTOM_CACHE_PATH="${BUILD_ROOT}/local-cache/swiftpm"
    (cd "${NOTIFIER_SRC}" && swift build --disable-sandbox -c release)
    "${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" --self-test
    NOTIFIER_BIN="${NOTIFIER_SRC}/.build/release/DJOneHubNotifier"
else
    # 构建 Universal：arm64 (SwiftPM) + x86_64 (xcrun swiftc)
    mkdir -p "${BUILD_ROOT}/local-cache/clang" "${BUILD_ROOT}/local-cache/swiftpm"
    export CLANG_MODULE_CACHE_PATH="${BUILD_ROOT}/local-cache/clang"
    export SWIFTPM_MODULECACHE_OVERRIDE="${BUILD_ROOT}/local-cache/clang"
    export SWIFTPM_CUSTOM_CACHE_PATH="${BUILD_ROOT}/local-cache/swiftpm"
    export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
    (cd "${NOTIFIER_SRC}" && swift build --disable-sandbox -c release)
    "${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" --self-test
    INTEL_ROOT="${BUILD_ROOT}/notifier-x86_64"
    rm -rf "${INTEL_ROOT}"
    mkdir -p "${INTEL_ROOT}/module-cache"
    cat > "${INTEL_ROOT}/CModemBridge.modulemap" <<EOF
module CModemBridge { header "${NOTIFIER_SRC}/Sources/CModemBridge/include/CModemBridge.h" export * }
EOF
    cat > "${INTEL_ROOT}/CUACProbe.modulemap" <<EOF
module CUACProbe { header "${NOTIFIER_SRC}/Sources/CUACProbe/include/CUACProbe.h" export * }
EOF
    cd "${NOTIFIER_SRC}"
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
    cd "${ROOT_DIR}"
    rm -f "${BUILD_ROOT}/DJOneHubNotifier-universal"
    lipo -create "${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" "${INTEL_ROOT}/DJOneHubNotifier" \
      -output "${BUILD_ROOT}/DJOneHubNotifier-universal"
    NOTIFIER_BIN="${BUILD_ROOT}/DJOneHubNotifier-universal"
fi

# 3. 组装 .app Bundle（方案 A：主程序为 DJOneHub，即重命名后的 DJOneHubNotifier）
echo "==> 组装 .app Bundle: ${OUT_APP}"
rm -rf "${OUT_APP}"
CONTENTS="${OUT_APP}/Contents"
MACOS_DIR="${CONTENTS}/MacOS"
RESOURCES_DIR="${CONTENTS}/Resources"

mkdir -p "${MACOS_DIR}" "${RESOURCES_DIR}/lib"

# Info.plist（来自 DJOneHubNotifier，CFBundleExecutable=DJOneHub）
cp "${NOTIFIER_SRC}/Info.plist" "${CONTENTS}/Info.plist"
plutil -replace CFBundleShortVersionString -string "${VERSION}" "${CONTENTS}/Info.plist"
plutil -replace CFBundleVersion -string "${VERSION}" "${CONTENTS}/Info.plist"
printf "APPL????" > "${CONTENTS}/PkgInfo"

# 主程序：DJOneHubNotifier 二进制重命名为 DJOneHub
cp "${NOTIFIER_BIN}" "${MACOS_DIR}/DJOneHub"
chmod 755 "${MACOS_DIR}/DJOneHub"

# 图标（使用上游 DJOneHubNotifier 的新图标）
cp "${NOTIFIER_SRC}/Resources/AppIcon.icns" "${RESOURCES_DIR}/AppIcon.icns"
cp "${NOTIFIER_SRC}/Resources/AppIcon.icns" "${RESOURCES_DIR}/DJOneHub.icns"

# Go 后端 & libusb（内嵌在 Resources 中，由 ProcessManager 在运行时启动）
cp "${RELEASE_DIR}/bin/djonehub-macos" "${RESOURCES_DIR}/djonehub-macos"
cp "${RELEASE_DIR}/lib/libusb-1.0.0.dylib" "${RESOURCES_DIR}/lib/libusb-1.0.0.dylib"
ln -sfn libusb-1.0.0.dylib "${RESOURCES_DIR}/lib/libusb-1.0.dylib"
chmod 755 "${RESOURCES_DIR}/djonehub-macos" "${RESOURCES_DIR}/lib/libusb-1.0.0.dylib"

# 4. 代码签名
echo "==> 对 Bundle 进行代码签名"
codesign --force --deep --sign - "${OUT_APP}"
codesign --verify --deep --strict "${OUT_APP}"
plutil -lint "${OUT_APP}/Contents/Info.plist"

echo "✅ App Bundle 构建完毕: ${OUT_APP}"
