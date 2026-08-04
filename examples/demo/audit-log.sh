#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
"$UNRING_BIN" log
echo
echo "Durable JSON records (newest first):"
"$UNRING_BIN" log --json

echo
echo "DIRECT AUDIT STORAGE (filesystem, not through unring log)"
audit_dir="$UNRING_STATE_DIR/logs"
if [ ! -d "$audit_dir" ]; then
    echo "audit directory is missing: $audit_dir" >&2
    exit 1
fi
find "$audit_dir" -type f -name '*.json' -exec basename {} \; | LC_ALL=C sort -r
newest_name=$(
    find "$audit_dir" -type f -name '*.json' -exec basename {} \; |
        LC_ALL=C sort -r | sed -n '1p'
)
if [ -z "$newest_name" ]; then
    echo "no durable audit records found" >&2
    exit 1
fi
echo
echo "Newest raw durable record (read directly from disk):"
sed -n '1,220p' "$audit_dir/$newest_name"
