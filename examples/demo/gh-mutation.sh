#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
PATH="$DEMO_DIR/bin:$PATH"
export PATH
set +e
"$UNRING_BIN" run --commit -- gh issue create \
    --repo acme/unring-demo --title 'local demo issue' --body 'no GitHub account is used'
status=$?
set -e
echo "wrapped command exit code: $status (approve with y to run the local fake gh)"
