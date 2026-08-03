#!/bin/sh

DEMO_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
REPO_DIR=$(CDPATH= cd "$DEMO_DIR/../.." && pwd)
DEMO_RUNTIME="$DEMO_DIR/runtime"
DEMO_DB_PORT=55439
DEMO_HTTPS_PORT=58443

if [ -n "${UNRING_BIN:-}" ]; then
    :
elif [ -x "$REPO_DIR/unring" ]; then
    UNRING_BIN="$REPO_DIR/unring"
elif command -v unring >/dev/null 2>&1; then
    UNRING_BIN=$(command -v unring)
else
    echo "unring is not built. From the repository root run: go build -o unring ./cmd/unring" >&2
    exit 1
fi

DATABASE_URL="postgresql:///unring_demo?host=$DEMO_RUNTIME&port=$DEMO_DB_PORT&user=postgres&sslmode=disable"
UNRING_STATE_DIR="$DEMO_RUNTIME/unring-state"
UNRING_ADAPTERS="$DEMO_DIR/adapter.yaml"
SSL_CERT_FILE="$DEMO_RUNTIME/fake-ca.pem"
GOCACHE="$REPO_DIR/.cache/go-build"
LC_ALL=C
LANG=C

export DEMO_DIR REPO_DIR DEMO_RUNTIME DEMO_DB_PORT DEMO_HTTPS_PORT
export UNRING_BIN UNRING_STATE_DIR UNRING_ADAPTERS SSL_CERT_FILE GOCACHE LC_ALL LANG
if [ "${UNRING_DEMO_NO_DATABASE:-0}" = "1" ]; then
    unset DATABASE_URL
else
    export DATABASE_URL
fi
