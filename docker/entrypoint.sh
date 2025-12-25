#!/bin/sh
set -eu

DATA_DIR="${DATA_DIR:-/data}"

if [ -n "${RCLONE_SYNCD_LISTEN:-}" ]; then
  LISTEN="$RCLONE_SYNCD_LISTEN"
elif [ -n "${LISTEN_ADDR:-}" ]; then
  LISTEN="$LISTEN_ADDR"
elif [ -n "${PORT:-}" ]; then
  LISTEN="0.0.0.0:${PORT}"
else
  LISTEN="0.0.0.0:8080"
fi

exec /usr/local/bin/rclone-syncd -listen "$LISTEN" -data "$DATA_DIR" "$@"

