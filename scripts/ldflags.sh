#!/usr/bin/env bash
# Prints the -ldflags for building the host, so build.sh and the release
# workflow stamp the same version: "<tailscale version>-tailtab-<build id>".
#
#   TAILTAB_VERSION   overrides the build id (a release tag such as 0.1.0);
#                     otherwise the short commit, plus -dirty for local edits.
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
