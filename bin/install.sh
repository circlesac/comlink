#!/bin/sh
# Installs the latest comlink release for this machine into $INSTALL_DIR (default ~/.local/bin).
set -eu

REPO="circlesac/comlink"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
	x86_64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
esac
case "$OS-$ARCH" in
	darwin-arm64 | darwin-amd64 | linux-amd64 | linux-arm64) ;;
	*)
		echo "Unsupported platform: $OS-$ARCH (Windows: download the zip from https://github.com/$REPO/releases)" >&2
		exit 1
		;;
esac

VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
	sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
if [ -z "$VERSION" ]; then
	echo "Could not determine the latest comlink version" >&2
	exit 1
fi

ARCHIVE="comlink-$OS-$ARCH.tar.gz"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/$ARCHIVE" "https://github.com/$REPO/releases/download/$VERSION/$ARCHIVE"
curl -fsSL -o "$TMP/checksums.txt" "https://github.com/$REPO/releases/download/$VERSION/checksums.txt"
EXPECTED=$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "$TMP/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then
	ACTUAL=$(sha256sum "$TMP/$ARCHIVE" | awk '{ print $1 }')
else
	ACTUAL=$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{ print $1 }')
fi
if [ -z "$EXPECTED" ] || [ "$EXPECTED" != "$ACTUAL" ]; then
	echo "Checksum mismatch for $ARCHIVE" >&2
	exit 1
fi

mkdir -p "$INSTALL_DIR"
tar -xzf "$TMP/$ARCHIVE" -C "$INSTALL_DIR" comlink
chmod +x "$INSTALL_DIR/comlink"
echo "Installed comlink $VERSION to $INSTALL_DIR/comlink"
