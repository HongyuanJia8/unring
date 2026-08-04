#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
(
    unset DATABASE_URL PGHOST PGPORT PGDATABASE PGUSER
    "$UNRING_BIN" run --discard -- sh -c '
        if env | grep -E "^(DATABASE_URL|PGHOST|PGPORT|PGDATABASE|PGUSER)="; then
            echo "child received a database connection variable in no-database mode" >&2
            exit 1
        fi
        echo "child check: no database connection variables were injected"
        exec curl -fsS "https://localhost:$1/health"
    ' sh "$DEMO_HTTPS_PORT"
)
