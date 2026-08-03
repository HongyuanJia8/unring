#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
decision=${1:---discard}
case "$decision" in --commit|--discard) ;; *) echo "usage: $0 [--commit|--discard]" >&2; exit 2 ;; esac
"$UNRING_BIN" run "$decision" -- curl -fsS \
    -H 'Content-Type: application/json' \
    -d '{"message":"stage me"}' \
    "https://localhost:$DEMO_HTTPS_PORT/stage"
echo
