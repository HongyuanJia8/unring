#!/bin/sh
set -eu

DEMO_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
DEMO_RUNTIME="$DEMO_DIR/runtime"
LC_ALL=C
LANG=C
export LC_ALL LANG

if [ -f "$DEMO_RUNTIME/fake-service.pid" ]; then
    service_pid=$(sed -n '1p' "$DEMO_RUNTIME/fake-service.pid")
    case "$service_pid" in
        ''|*[!0-9]*) echo "ignoring invalid fake-service.pid" >&2 ;;
        *)
            kill "$service_pid" 2>/dev/null || true
            wait "$service_pid" 2>/dev/null || true
            ;;
    esac
fi
if [ -s "$DEMO_DIR/pgdata/PG_VERSION" ] && command -v pg_ctl >/dev/null 2>&1; then
    pg_ctl -D "$DEMO_DIR/pgdata" -m fast stop >/dev/null 2>&1 || true
fi

rm -rf "$DEMO_DIR/pgdata" "$DEMO_RUNTIME"
rm -f "$DEMO_DIR/pg.log"
echo "Stopped the demo and removed pgdata, logs, certificates, request records, and audit state."
