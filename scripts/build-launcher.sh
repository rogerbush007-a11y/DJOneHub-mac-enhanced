#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
LAUNCHER_SRC="${ROOT_DIR}/macos/DJOneHubLauncher"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-launcher-build"
mkdir -p "${BUILD_ROOT}/cache" "${BUILD_ROOT}/swiftpm"

export CLANG_MODULE_CACHE_PATH="${BUILD_ROOT}/cache"
export SWIFTPM_MODULECACHE_OVERRIDE="${BUILD_ROOT}/cache"
export SWIFTPM_CUSTOM_CACHE_PATH="${BUILD_ROOT}/swiftpm"

ARCH=${1:-universal}
OUT_BIN=${2:-"${BUILD_ROOT}/DJOneHubLauncher"}

cd "${LAUNCHER_SRC}"

if [ "${ARCH}" = "arm64" ]; then
    echo "==> 构建 arm64 DJOneHubLauncher"
    xcrun swift build --disable-sandbox -c release
    cp "${LAUNCHER_SRC}/.build/release/DJOneHubLauncher" "${OUT_BIN}"
else
    echo "==> 构建 Universal DJOneHubLauncher (arm64 + x86_64)"
    xcrun swift build --disable-sandbox -c release
    xcrun swiftc -O -target x86_64-apple-macosx13.0 Sources/DJOneHubLauncher/*.swift -o "${BUILD_ROOT}/launcher-x86_64"
    lipo -create "${LAUNCHER_SRC}/.build/release/DJOneHubLauncher" "${BUILD_ROOT}/launcher-x86_64" -output "${OUT_BIN}"
fi

chmod 755 "${OUT_BIN}"
echo "✅ DJOneHubLauncher 已生成: ${OUT_BIN}"
