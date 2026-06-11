#!/bin/sh
# Builds a portable (static-deps) uuu for macOS and drops it into
# src-tauri/binaries/uuu-<rust-triple> as the Tauri sidecar.
#
# All homebrew libraries are linked statically via a symlink farm that only
# exposes .a files; tinyxml2 comes from the vendored submodule. The result
# depends only on system frameworks and runs on a clean Mac.
#
# Prereqs: brew install libusb zstd zlib openssl@3 tinyxml2 pkg-config cmake
set -e

cd "$(dirname "$0")/.."
ARCH=$(uname -m)
case "$ARCH" in
  arm64)  TRIPLE=aarch64-apple-darwin ;;
  x86_64) TRIPLE=x86_64-apple-darwin ;;
  *) echo "unsupported arch $ARCH"; exit 1 ;;
esac
BREW=$(brew --prefix)
FARM=$(mktemp -d)

git submodule update --init tinyxml2
cmake -S tinyxml2 -B tinyxml2/build -DBUILD_SHARED_LIBS=OFF \
  -DCMAKE_BUILD_TYPE=Release -Dtinyxml2_BUILD_TESTING=OFF
cmake --build tinyxml2/build -j8

ln -sf "$BREW/opt/libusb/lib/libusb-1.0.a" \
       "$BREW/opt/zstd/lib/libzstd.a" \
       "$BREW/opt/zlib/lib/libz.a" \
       "$PWD/tinyxml2/build/libtinyxml2.a" \
       "$FARM/"

PKG_CONFIG_PATH="$BREW/lib/pkgconfig:$BREW/opt/openssl@3/lib/pkgconfig:$BREW/opt/zlib/lib/pkgconfig" \
cmake -B build-static -DCMAKE_BUILD_TYPE=Release \
  -DOPENSSL_USE_STATIC_LIBS=TRUE \
  -DCMAKE_EXE_LINKER_FLAGS="-L$FARM -framework CoreFoundation -framework IOKit -framework Security"
cmake --build build-static -j8

mkdir -p uuu-gui/src-tauri/binaries
cp build-static/uuu/uuu "uuu-gui/src-tauri/binaries/uuu-$TRIPLE"
rm -rf "$FARM"

echo "OK: uuu-gui/src-tauri/binaries/uuu-$TRIPLE"
otool -L "uuu-gui/src-tauri/binaries/uuu-$TRIPLE"
