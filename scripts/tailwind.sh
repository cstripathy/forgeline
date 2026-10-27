#!/bin/sh
# Prints the path of the pinned Tailwind CSS standalone CLI, downloading and verifying it first.
# The checksums below are from the release's sha256sums.txt (tailwindlabs/tailwindcss v4.3.3).
# The binary lands in .tools/, which git ignores. No Node is needed.
set -eu

VERSION=v4.3.3
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)         ASSET=tailwindcss-linux-x64;   SUM=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a ;;
  Linux-aarch64|Linux-arm64) ASSET=tailwindcss-linux-arm64; SUM=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195 ;;
  Darwin-arm64)         ASSET=tailwindcss-macos-arm64; SUM=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d ;;
  Darwin-x86_64)        ASSET=tailwindcss-macos-x64;   SUM=7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1 ;;
  *) echo "scripts/tailwind.sh: no pinned Tailwind CLI for $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/.tools/tailwindcss-$VERSION"

check() { [ "$(openssl dgst -sha256 "$1" 2>/dev/null | awk '{print $NF}')" = "$SUM" ] || [ "$(sha256sum "$1" 2>/dev/null | awk '{print $1}')" = "$SUM" ] || [ "$(shasum -a 256 "$1" 2>/dev/null | awk '{print $1}')" = "$SUM" ]; }

if [ -x "$BIN" ] && check "$BIN"; then
  echo "$BIN"
  exit 0
fi

mkdir -p "$ROOT/.tools"
TMP=$(mktemp "$ROOT/.tools/dl.XXXXXX")
trap 'rm -f "$TMP"' EXIT
curl -fsSL -o "$TMP" "https://github.com/tailwindlabs/tailwindcss/releases/download/$VERSION/$ASSET"
if ! check "$TMP"; then
  echo "scripts/tailwind.sh: checksum mismatch for $ASSET $VERSION; refusing to use it" >&2
  exit 1
fi
chmod +x "$TMP"
mv "$TMP" "$BIN"
trap - EXIT
echo "$BIN"
