#!/usr/bin/env bash
# usage: ./scripts/load_test.sh <concurrency>
# Sends 200 POST /run requests at the given concurrency using hey.

set -euo pipefail

CONCURRENCY="${1:-10}"
URL="${GOBOXD_URL:-http://localhost:8080/run}"
PAYLOAD='{"language":"py3","source":"print(\"hi\")","tests":[{"stdin":"","expected_stdout":"hi\n"}]}'

if ! command -v hey &>/dev/null; then
    echo "hey not found. Install it with:"
    echo "  go install github.com/rakyll/hey@latest"
    echo "Then ensure \$(go env GOPATH)/bin is in your PATH."
    exit 1
fi

echo "=== load test: c=${CONCURRENCY}, n=200, url=${URL} ==="
hey -n 200 -c "$CONCURRENCY" \
    -m POST \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD" \
    "$URL"
