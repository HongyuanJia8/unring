#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
set +e
"$UNRING_BIN" run --discard -- curl -fsS \
    -H 'Content-Type: application/json' \
    -d '{"action":"needs a real response"}' \
    "https://localhost:$DEMO_HTTPS_PORT/approval"
status=$?
set -e
echo
echo "wrapped command exit code: $status (decline first, then rerun and approve with y)"
