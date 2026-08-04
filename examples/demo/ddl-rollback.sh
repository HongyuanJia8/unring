#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
"$UNRING_BIN" run --discard -- psql -X -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE demo_ddl_rollback (id integer PRIMARY KEY, note text NOT NULL);
INSERT INTO demo_ddl_rollback VALUES (1, 'visible inside the wrapped transaction');
SELECT * FROM demo_ddl_rollback;
SQL
