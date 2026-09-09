#!/usr/bin/env bash
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

echo "== distribution scripts =="
"$node" --test scripts/test/smoke.test.js

if [ "${OS:-}" = Windows_NT ]; then
  powershell -NoProfile -File scripts/test/install.test.ps1
else
  "$node" --test scripts/test/release.test.js
fi
