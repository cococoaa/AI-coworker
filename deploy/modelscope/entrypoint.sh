#!/bin/sh
set -eu

set -- -addr 0.0.0.0:7860 -config config.example.json
case "${REIMBURSE_MODE:-demo}" in
  demo) set -- "$@" -demo-synthetic -upload-dir - ;;
  live) ;;
  *) echo 'REIMBURSE_MODE must be demo or live.' >&2; exit 1 ;;
esac
exec /app/reimburse "$@"
