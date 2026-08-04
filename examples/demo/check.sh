#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"

echo "REAL DATABASE (direct connection, not through unring)"
psql "$DATABASE_URL" -X -P pager=off -c \
    "SELECT id, name FROM demo_accounts ORDER BY id; SELECT id, account_id, name FROM demo_projects ORDER BY id; SELECT count(*) AS event_count FROM demo_events; SELECT to_regclass('public.demo_ddl_rollback') AS ddl_table; SELECT last_vacuum FROM pg_stat_user_tables WHERE relname = 'demo_accounts';"

echo
echo "REAL FAKE-SERVICE REQUEST LOG"
if [ -s "$DEMO_RUNTIME/received.ndjson" ]; then
    nl -ba "$DEMO_RUNTIME/received.ndjson"
else
    echo "(no HTTPS mutations received)"
fi

echo
echo "REAL FAKE-gh INVOCATION LOG"
if [ -s "$DEMO_RUNTIME/gh-received.log" ]; then
    nl -ba "$DEMO_RUNTIME/gh-received.log"
else
    echo "(fake gh has not run)"
fi
