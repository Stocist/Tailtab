#!/usr/bin/env bash
# Runs everything that can be checked without a browser: the Go host and the
# extension's node tests.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go="${GO:-go}"
node="${NODE:-node}"

echo "== go vet =="
"$go" vet ./...

echo "== go test =="
"$go" test ./...

echo "== extension =="
"$node" extension/test/background.test.js
