#!/usr/bin/env bash
#
# build.sh - Cross-compile djonehub (Linux) for arm64 / armv7 / amd64
#
# Output artifact naming matches exactly what luci-app-djonehub downloads:
#     djonehub_<version>_linux_<arch>     (arch in: arm64, armv7, amd64)
#
# Usage:
#     ./build.sh [version]            e.g.  ./build.sh v0.1.0
#     VERSION=v0.1.0 OUT_DIR=dist ./build.sh
#
# Prereqs: Go 1.26+ (GOTOOLCHAIN=auto will fetch it), git (for go modules).
# No CGO / libusb needed on Linux: the DJI USB-AT path uses go.bug.st/serial.
#
set -euo pipefail

VERSION="${1:-${VERSION:-v0.1.0}}"
OUT_DIR="${OUT_DIR:-dist}"
GO_BIN="${GO:-go}"

export CGO_ENABLED=0
export GOTOOLCHAIN=auto

mkdir -p "$OUT_DIR"

# host arch -> GOARCH
declare -A ARCH_MAP=( [arm64]=arm64 [armv7]=arm [amd64]=amd64 )

for arch in arm64 armv7 amd64; do
  goarch="${ARCH_MAP[$arch]}"
  out="$OUT_DIR/djonehub_${VERSION}_linux_${arch}"
  echo ">> building linux/$goarch -> $out"
  GOOS=linux GOARCH="$goarch" \
    "$GO_BIN" build -mod=mod -o "$out" ./cmd/djonehub-macos
  chmod +x "$out"
done

echo
echo "Done. Artifacts in $OUT_DIR :"
ls -lh "$OUT_DIR"/djonehub_${VERSION}_linux_* 2>/dev/null || ls -lh "$OUT_DIR"
