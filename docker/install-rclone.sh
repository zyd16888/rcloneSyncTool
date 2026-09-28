#!/bin/sh
set -eu

version="$1"
arch="$2"
case "$arch" in
  amd64|arm64) platform="linux-${arch}" ;;
  arm) platform="linux-arm-${3:-v6}" ;;
  *) echo "Unsupported rclone architecture: $arch" >&2; exit 1 ;;
esac

# Resolve assets from an immutable release tag. Rebuilding the same app release
# must not silently switch its rclone implementation.
release="$(curl -fsSL "https://api.github.com/repos/wiserain/rclone/releases/tags/${version}")"
asset="$(printf '%s' "$release" | jq -r --arg platform "$platform" '.assets[] | select(.name | endswith($platform + ".zip")) | .browser_download_url' | head -n 1)"
if [ -z "$asset" ]; then echo "No rclone asset for $version / $platform" >&2; exit 1; fi
curl -fsSL "$asset" -o /tmp/rclone.zip
unzip -q /tmp/rclone.zip -d /tmp/rclone-install
binary="$(find /tmp/rclone-install -type f -name rclone | head -n 1)"
if [ -z "$binary" ]; then echo "rclone binary missing from release archive" >&2; exit 1; fi
install -m 0755 "$binary" /usr/local/bin/rclone
rm -rf /tmp/rclone-install /tmp/rclone.zip
