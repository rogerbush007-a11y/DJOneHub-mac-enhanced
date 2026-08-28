#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v1.2.9}
PACKAGE_NAME="DJOneHub-macOS-universal-${VERSION}"
STAGE_ROOT="${ROOT_DIR}/dist/release"
STAGE_DIR="${STAGE_ROOT}/${PACKAGE_NAME}"
ARCHIVE="${STAGE_ROOT}/${PACKAGE_NAME}.zip"
CHECKSUM="${ARCHIVE}.sha256"
LIBUSB_VERSION=1.0.30
LIBUSB_SHA256=fea36f34f9156400209595e300840767ab1a385ede1dc7ee893015aea9c6dbaf
LIBUSB_URL="https://github.com/libusb/libusb/releases/download/v${LIBUSB_VERSION}/libusb-${LIBUSB_VERSION}.tar.bz2"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-macos-package-universal"
LIBUSB_ARCHIVE="${BUILD_ROOT}/libusb-${LIBUSB_VERSION}.tar.bz2"
LIBUSB_SOURCE="${BUILD_ROOT}/libusb-source"
LIBUSB_ARM="${BUILD_ROOT}/libusb-arm64"
LIBUSB_X86="${BUILD_ROOT}/libusb-x86_64"
PC_SHIM="${BUILD_ROOT}/pc-shim"

if ! command -v go >/dev/null 2>&1; then echo "Go is required to build the release package." >&2; exit 1; fi
if ! command -v curl >/dev/null 2>&1; then echo "curl is required to download the official libusb source archive." >&2; exit 1; fi
if ! command -v pkg-config >/dev/null 2>&1; then echo "pkg-config is required on the build Mac." >&2; exit 1; fi
if ! command -v lipo >/dev/null 2>&1; then echo "lipo is required to build a universal package." >&2; exit 1; fi

rm -rf "${STAGE_DIR}"
mkdir -p "${STAGE_DIR}/bin" "${STAGE_DIR}/lib" "${STAGE_DIR}/licenses"
rm -rf "${LIBUSB_ARM}" "${LIBUSB_X86}"
mkdir -p "${BUILD_ROOT}" "${LIBUSB_ARM}/lib" "${LIBUSB_X86}/lib"

if [ ! -f "${LIBUSB_ARCHIVE}" ]; then
  curl -fL "${LIBUSB_URL}" -o "${LIBUSB_ARCHIVE}"
fi
ACTUAL_SHA256=$(shasum -a 256 "${LIBUSB_ARCHIVE}" | awk '{print $1}')
if [ "${ACTUAL_SHA256}" != "${LIBUSB_SHA256}" ]; then
  echo "libusb source checksum mismatch." >&2
  exit 1
fi

rm -rf "${LIBUSB_SOURCE}"
mkdir -p "${LIBUSB_SOURCE}"
tar -xjf "${LIBUSB_ARCHIVE}" -C "${LIBUSB_SOURCE}" --strip-components=1
(
  cd "${LIBUSB_SOURCE}"
  MACOSX_DEPLOYMENT_TARGET=13.0 ./configure \
    --prefix="${BUILD_ROOT}/libusb-prefix" \
    --disable-static \
    --enable-shared \
    --disable-dependency-tracking >/dev/null
  sed -i '' 's/#define HAVE_PIPE2 1/\/\* #undef HAVE_PIPE2 \*\//' config.h
)
mkdir -p "${BUILD_ROOT}/libusb-prefix/include/libusb-1.0"
cp "${LIBUSB_SOURCE}/libusb/libusb.h" "${BUILD_ROOT}/libusb-prefix/include/libusb-1.0/libusb.h"

build_libusb() {
  arch=$1
  out=$2
  objects="${BUILD_ROOT}/libusb-${arch}-objects"
  mkdir -p "${objects}"
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
    object="${objects}/$(basename "${source}" .c).o"
    clang -arch "${arch}" -mmacosx-version-min=13.0 -DHAVE_CONFIG_H \
      -I"${LIBUSB_SOURCE}" -I"${LIBUSB_SOURCE}/libusb" -fPIC \
      -c "${LIBUSB_SOURCE}/${source}" -o "${object}"
  done
  clang -arch "${arch}" -mmacosx-version-min=13.0 -dynamiclib \
    -install_name "@executable_path/../lib/libusb-1.0.0.dylib" \
    -compatibility_version 7.0.0 -current_version 7.0.0 \
    -o "${out}/lib/libusb-1.0.0.dylib" \
    "${objects}"/*.o \
    -framework IOKit -framework CoreFoundation -framework Security -lobjc
  ln -sfn libusb-1.0.0.dylib "${out}/lib/libusb-1.0.dylib"
}

build_libusb arm64 "${LIBUSB_ARM}"
build_libusb x86_64 "${LIBUSB_X86}"
lipo -create "${LIBUSB_ARM}/lib/libusb-1.0.0.dylib" "${LIBUSB_X86}/lib/libusb-1.0.0.dylib" \
  -output "${STAGE_DIR}/lib/libusb-1.0.0.dylib"
ln -sfn libusb-1.0.0.dylib "${STAGE_DIR}/lib/libusb-1.0.dylib"

# x86_64 构建用 pkg-config shim，避免被系统 Homebrew 的 arm64 libusb 干扰
mkdir -p "${PC_SHIM}"
cat > "${PC_SHIM}/pkg-config" <<EOF
#!/bin/sh
case "\$*" in
  *libusb-1.0*)
    out=""
    if [ "\${PKG_ARCH:-arm64}" = "x86_64" ] || [ "\${PKG_ARCH:-arm64}" = "amd64" ]; then libdir="${LIBUSB_X86}/lib"; else libdir="${LIBUSB_ARM}/lib"; fi
    case "\$*" in *--cflags*) out="-I${LIBUSB_SOURCE}/libusb \$out" ;; esac
    case "\$*" in *--libs*) out="-L\${libdir} -lusb-1.0 \$out" ;; esac
    [ -n "\$out" ] && echo "\$out"
    exit 0
    ;;
esac
exec /usr/bin/pkg-config "\$@"
EOF
chmod 755 "${PC_SHIM}/pkg-config"

cd "${ROOT_DIR}"
build_go() {
  arch=$1
  cache="${BUILD_ROOT}/go-cache-${arch}"
  rm -rf "${cache}"
  mkdir -p "${cache}"
  PATH="${PC_SHIM}:$PATH" PKG_ARCH="${arch}" GOCACHE="${cache}" \
    PKG_CONFIG_PATH="" \
    MACOSX_DEPLOYMENT_TARGET=13.0 CGO_ENABLED=1 GOOS=darwin GOARCH="${arch}" \
    go build -p 2 -trimpath -buildvcs=false -ldflags="-s -w" \
    -o "${BUILD_ROOT}/djonehub-${arch}" ./cmd/djonehub-macos
}

build_go arm64
build_go amd64
lipo -create "${BUILD_ROOT}/djonehub-arm64" "${BUILD_ROOT}/djonehub-amd64" \
  -output "${STAGE_DIR}/bin/djonehub-macos"

cp "${ROOT_DIR}/packaging/README.md" "${STAGE_DIR}/README.md"
cp "${ROOT_DIR}/LICENSE" "${STAGE_DIR}/LICENSE"
cp "${LIBUSB_SOURCE}/COPYING" "${STAGE_DIR}/licenses/libusb-COPYING"
cp "${ROOT_DIR}/packaging/THIRD_PARTY_NOTICES.md" "${STAGE_DIR}/THIRD_PARTY_NOTICES.md"

# Public packages must never redistribute the module-side call runtime.
if find "${STAGE_DIR}" -type f \( -name '*.ko' -o -name '*.armv7' \) | grep -q .; then
  echo "Public package unexpectedly contains a module-side runtime." >&2
  exit 1
fi

chmod 755 "${STAGE_DIR}/bin/djonehub-macos" "${STAGE_DIR}/lib/libusb-1.0.0.dylib"
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
