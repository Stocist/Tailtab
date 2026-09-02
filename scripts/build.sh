#!/usr/bin/env bash
# Builds the tailtab native host and both unpacked extensions.
#
#   bin/tailtab                       the native-messaging host
#   extension/dist/chromium/          load this in Edge
#   extension/dist/firefox/           load this in Zen
#
# Each dist directory gets exactly one manifest, named manifest.json, because a
# browser will not load a directory holding a manifest it cannot parse.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go="${GO:-go}"
# Stamp the embedded Tailscale version so the admin console and the login page
# show "1.102.3-tailtab-<commit>" instead of the "-dev" string tsnet reports for
# an unstamped build. The version is whatever go.mod pins.
ts_version="$("$go" list -m -f '{{.Version}}' tailscale.com)"
ts_version="${ts_version#v}"
commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
ldflags="-X tailscale.com/version.longStamp=${ts_version}-tailtab-${commit} -X tailscale.com/version.shortStamp=${ts_version}"
echo "building bin/tailtab (tailscale ${ts_version}-tailtab-${commit})"
"$go" build -ldflags "$ldflags" -o bin/tailtab ./cmd/tailtab

src="extension"
dist="$src/dist"
# Copied by name, so extension/test/ and anything else alongside the sources
# never reaches a browser.
shared=(background.js rules.js popup.html popup.js)

rm -rf "$dist"
for target in chromium firefox; do
  out="$dist/$target"
  mkdir -p "$out/icons"
  for f in "${shared[@]}"; do
    cp "$src/$f" "$out/$f"
  done
  cp "$src"/icons/*.png "$out/icons/"
done

cp "$src/manifest.chromium.json" "$dist/chromium/manifest.json"
cp "$src/manifest.firefox.json" "$dist/firefox/manifest.json"

echo "built $dist/chromium and $dist/firefox"
echo "run ./scripts/test.sh to check the host and the extension"
