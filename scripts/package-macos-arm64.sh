#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-dev}
PACKAGE_NAME="DJOneHub-macOS-arm64-${VERSION}"
STAGE_ROOT="${ROOT_DIR}/dist/release"
STAGE_DIR="${STAGE_ROOT}/${PACKAGE_NAME}"
ARCHIVE="${STAGE_ROOT}/${PACKAGE_NAME}.zip"
CHECKSUM="${ARCHIVE}.sha256"
LIBUSB_VERSION=1.0.30
LIBUSB_SHA256=fea36f34f9156400209595e300840767ab1a385ede1dc7ee893015aea9c6dbaf
LIBUSB_URL="https://github.com/libusb/libusb/releases/download/v${LIBUSB_VERSION}/libusb-${LIBUSB_VERSION}.tar.bz2"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-macos-package-arm64"
LIBUSB_ARCHIVE="${BUILD_ROOT}/libusb-${LIBUSB_VERSION}.tar.bz2"
LIBUSB_SOURCE="${BUILD_ROOT}/libusb-source"
LIBUSB_PREFIX="${BUILD_ROOT}/libusb-prefix"
LIBUSB_OBJECTS="${BUILD_ROOT}/libusb-objects"

if [ "$(uname -m)" != "arm64" ]; then
  echo "This packaging script currently requires an Apple Silicon Mac." >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to build the release package." >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to download the official libusb source archive." >&2
  exit 1
fi
if ! command -v pkg-config >/dev/null 2>&1; then
  echo "pkg-config is required on the build Mac." >&2
  exit 1
fi

rm -rf "${STAGE_DIR}"
mkdir -p "${STAGE_DIR}/bin" "${STAGE_DIR}/lib" "${STAGE_DIR}/licenses"
mkdir -p "${BUILD_ROOT}"

if [ ! -f "${LIBUSB_ARCHIVE}" ]; then
  curl -fL "${LIBUSB_URL}" -o "${LIBUSB_ARCHIVE}"
fi
ACTUAL_SHA256=$(shasum -a 256 "${LIBUSB_ARCHIVE}" | awk '{print $1}')
if [ "${ACTUAL_SHA256}" != "${LIBUSB_SHA256}" ]; then
  echo "libusb source checksum mismatch." >&2
  exit 1
fi

rm -rf "${LIBUSB_SOURCE}" "${LIBUSB_PREFIX}" "${LIBUSB_OBJECTS}"
mkdir -p "${LIBUSB_SOURCE}" "${LIBUSB_PREFIX}/lib" "${LIBUSB_PREFIX}/include/libusb-1.0" "${LIBUSB_OBJECTS}"
tar -xjf "${LIBUSB_ARCHIVE}" -C "${LIBUSB_SOURCE}" --strip-components=1

(
  cd "${LIBUSB_SOURCE}"
  MACOSX_DEPLOYMENT_TARGET=13.0 ./configure \
    --prefix="${LIBUSB_PREFIX}" \
    --disable-static \
    --enable-shared \
    --disable-dependency-tracking >/dev/null
  # The newest SDK exposes pipe2, but macOS 13 does not. Use libusb's portable pipe path.
  sed -i '' 's/#define HAVE_PIPE2 1/\/\* #undef HAVE_PIPE2 \*\//' config.h
)

for source in \
  libusb/core.c \
  libusb/descriptor.c \
  libusb/hotplug.c \
  libusb/io.c \
  libusb/strerror.c \
  libusb/sync.c \
  libusb/os/events_posix.c \
  libusb/os/threads_posix.c \
  libusb/os/darwin_usb.c
do
  object="${LIBUSB_OBJECTS}/$(basename "${source}" .c).o"
  clang -arch arm64 -mmacosx-version-min=13.0 -DHAVE_CONFIG_H \
    -I"${LIBUSB_SOURCE}" -I"${LIBUSB_SOURCE}/libusb" -fPIC \
    -c "${LIBUSB_SOURCE}/${source}" -o "${object}"
done

clang -arch arm64 -mmacosx-version-min=13.0 -dynamiclib \
  -install_name "@executable_path/../lib/libusb-1.0.0.dylib" \
  -compatibility_version 7.0.0 -current_version 7.0.0 \
  -o "${LIBUSB_PREFIX}/lib/libusb-1.0.0.dylib" \
  "${LIBUSB_OBJECTS}"/*.o \
  -framework IOKit -framework CoreFoundation -framework Security -lobjc
ln -s libusb-1.0.0.dylib "${LIBUSB_PREFIX}/lib/libusb-1.0.dylib"
cp "${LIBUSB_SOURCE}/libusb/libusb.h" "${LIBUSB_PREFIX}/include/libusb-1.0/libusb.h"

cd "${ROOT_DIR}"
GOCACHE="${BUILD_ROOT}/go-cache"
rm -rf "${GOCACHE}"
mkdir -p "${GOCACHE}"
export GOCACHE
SDKROOT="$(xcrun --show-sdk-path)" CC="$(xcrun -f clang)" \
PKG_CONFIG_PATH="${LIBUSB_SOURCE}" \
MACOSX_DEPLOYMENT_TARGET=13.0 CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -p 2 \
  -trimpath -buildvcs=false -ldflags="-s -w" \
  -o "${STAGE_DIR}/bin/djonehub-macos" ./cmd/djonehub-macos

cp "${LIBUSB_PREFIX}/lib/libusb-1.0.0.dylib" "${STAGE_DIR}/lib/libusb-1.0.0.dylib"
cp "${ROOT_DIR}/packaging/djonehub" "${STAGE_DIR}/djonehub"
cp "${ROOT_DIR}/packaging/install" "${STAGE_DIR}/install"
cp "${ROOT_DIR}/packaging/README.md" "${STAGE_DIR}/README.md"
cp "${ROOT_DIR}/LICENSE" "${STAGE_DIR}/LICENSE"
cp "${LIBUSB_SOURCE}/COPYING" "${STAGE_DIR}/licenses/libusb-COPYING"
cp "${ROOT_DIR}/packaging/THIRD_PARTY_NOTICES.md" "${STAGE_DIR}/THIRD_PARTY_NOTICES.md"

chmod 755 "${STAGE_DIR}/djonehub" "${STAGE_DIR}/install" "${STAGE_DIR}/bin/djonehub-macos" "${STAGE_DIR}/lib/libusb-1.0.0.dylib"
codesign --force --sign - "${STAGE_DIR}/lib/libusb-1.0.0.dylib"
codesign --force --sign - "${STAGE_DIR}/bin/djonehub-macos"

if otool -L "${STAGE_DIR}/bin/djonehub-macos" | grep -q '/opt/homebrew\|/usr/local\|/Cellar/'; then
  echo "Release binary still contains a package-manager dependency." >&2
  exit 1
fi

find "${STAGE_DIR}" -name '._*' -delete
rm -f "${ARCHIVE}" "${CHECKSUM}"
ditto -c -k --keepParent --norsrc --noextattr --noqtn --noacl "${STAGE_DIR}" "${ARCHIVE}"
(
  cd "${STAGE_ROOT}"
  shasum -a 256 "$(basename -- "${ARCHIVE}")" >"$(basename -- "${CHECKSUM}")"
)

echo "Release directory: ${STAGE_DIR}"
echo "Release archive:   ${ARCHIVE}"
echo "Checksum:          ${CHECKSUM}"
