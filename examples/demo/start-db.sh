#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"

for command_name in initdb pg_ctl createdb psql; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        echo "$command_name is required for the demo" >&2
        exit 1
    fi
done

mkdir -p "$DEMO_RUNTIME"
if [ ! -s "$DEMO_DIR/pgdata/PG_VERSION" ]; then
    initdb -D "$DEMO_DIR/pgdata" -U postgres -A trust --locale=C >/dev/null
fi
if ! pg_ctl -D "$DEMO_DIR/pgdata" status >/dev/null 2>&1; then
    pg_ctl -D "$DEMO_DIR/pgdata" \
        -o "-p $DEMO_DB_PORT -k $DEMO_RUNTIME -h ''" \
        -l "$DEMO_DIR/pg.log" start >/dev/null
fi
if ! psql "$DATABASE_URL" -Atqc 'SELECT 1' >/dev/null 2>&1; then
    createdb -h "$DEMO_RUNTIME" -p "$DEMO_DB_PORT" -U postgres unring_demo
fi
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$DEMO_DIR/schema.sql" >/dev/null
echo "PostgreSQL demo is running and reset to its known initial state."
