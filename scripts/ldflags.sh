#!/usr/bin/env bash
# Centralized stamping keeps local and release host versions synchronized.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go="${GO:-go}"
ts_version="$(cd "$root" && "$go" list -m -f '{{.Version}}' tailscale.com)"
ts_version="${ts_version#v}"
if [ -n "${TAILTAB_VERSION:-}" ]; then
  build_id="$TAILTAB_VERSION"
else
  commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  build_id="${commit}$(git -C "$root" diff --quiet 2>/dev/null || echo -dirty)"
fi
echo "-X tailscale.com/version.longStamp=${ts_version}-tailtab-${build_id} -X tailscale.com/version.shortStamp=${ts_version}"
