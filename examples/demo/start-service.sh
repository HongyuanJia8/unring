#!/bin/sh
set -eu

. "$(CDPATH= cd "$(dirname "$0")" && pwd)/common.sh"
mkdir -p "$DEMO_RUNTIME"
if [ -f "$DEMO_RUNTIME/fake-service.pid" ] && kill -0 "$(sed -n '1p' "$DEMO_RUNTIME/fake-service.pid")" 2>/dev/null; then
    echo "Fake HTTPS service is already running."
    exit 0
fi

go build -o "$DEMO_RUNTIME/fake-service" "$DEMO_DIR/fake-service.go"
"$DEMO_RUNTIME/fake-service" -runtime "$DEMO_RUNTIME" -port "$DEMO_HTTPS_PORT" \
    >"$DEMO_RUNTIME/fake-service.log" 2>&1 &
service_pid=$!
printf '%s\n' "$service_pid" > "$DEMO_RUNTIME/fake-service.pid"

attempt=0
while [ "$attempt" -lt 50 ]; do
    if [ -s "$SSL_CERT_FILE" ] && curl --noproxy '*' --cacert "$SSL_CERT_FILE" -fsS \
        "https://localhost:$DEMO_HTTPS_PORT/health" >/dev/null 2>&1; then
        echo "Fake HTTPS service is running."
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
echo "fake HTTPS service did not start; inspect $DEMO_RUNTIME/fake-service.log" >&2
exit 1
