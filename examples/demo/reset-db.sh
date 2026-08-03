#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$DEMO_DIR/schema.sql" >/dev/null
echo "Database reset to the known initial state."
