#!/bin/sh
set -eu

DEMO_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
DEMO_RUNTIME="$DEMO_DIR/runtime"
LC_ALL=C
LANG=C
export LC_ALL LANG

wait_for_exit() {
    process_name=$1
    process_pid=$2
    attempt=0
    while kill -0 "$process_pid" 2>/dev/null; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 50 ]; then
            echo "could not stop $process_name (pid $process_pid); leaving demo state in place" >&2
            return 1
        fi
        sleep 0.1
    done
}

if [ -f "$DEMO_RUNTIME/fake-service.pid" ]; then
    service_pid=$(sed -n '1p' "$DEMO_RUNTIME/fake-service.pid")
    case "$service_pid" in
        ''|*[!0-9]*)
            echo "invalid fake-service.pid; leaving demo state in place" >&2
            exit 1
            ;;
        *)
            if kill -0 "$service_pid" 2>/dev/null; then
                if ! kill "$service_pid" 2>/dev/null; then
                    echo "could not signal fake HTTPS service (pid $service_pid); leaving demo state in place" >&2
                    exit 1
                fi
                if ! wait_for_exit "fake HTTPS service" "$service_pid"; then
                    exit 1
                fi
            fi
            ;;
    esac
fi
if [ -s "$DEMO_DIR/pgdata/PG_VERSION" ]; then
    if ! command -v pg_ctl >/dev/null 2>&1; then
        echo "pg_ctl is required to verify PostgreSQL has stopped; leaving demo state in place" >&2
        exit 1
    fi
    set +e
    pg_ctl -D "$DEMO_DIR/pgdata" status >/dev/null 2>&1
    postgres_status=$?
    set -e
    case "$postgres_status" in
        0)
            if ! pg_ctl -D "$DEMO_DIR/pgdata" -m fast stop >/dev/null 2>&1; then
                echo "pg_ctl could not stop the demo PostgreSQL server; leaving demo state in place" >&2
                exit 1
            fi
            set +e
            pg_ctl -D "$DEMO_DIR/pgdata" status >/dev/null 2>&1
            postgres_status=$?
            set -e
            if [ "$postgres_status" -ne 3 ]; then
                echo "could not verify that demo PostgreSQL stopped; leaving demo state in place" >&2
                exit 1
            fi
            ;;
        3) ;;
        *)
            echo "could not determine demo PostgreSQL status; leaving demo state in place" >&2
            exit 1
            ;;
    esac
fi

rm -rf "$DEMO_DIR/pgdata" "$DEMO_RUNTIME"
rm -f "$DEMO_DIR/pg.log"
echo "Stopped the demo and removed pgdata, logs, certificates, request records, and audit state."
