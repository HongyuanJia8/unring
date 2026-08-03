#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
set +e
"$UNRING_BIN" run --discard -- psql -X -v ON_ERROR_STOP=1 -c 'VACUUM demo_accounts'
status=$?
set -e
echo "wrapped command exit code: $status (approve with y to run VACUUM; anything else declines)"
