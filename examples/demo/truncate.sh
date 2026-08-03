#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
"$UNRING_BIN" run --discard -- psql -X -v ON_ERROR_STOP=1 <<'SQL'
TRUNCATE demo_events;
SELECT count(*) AS visible_inside_unring FROM demo_events;
SQL
