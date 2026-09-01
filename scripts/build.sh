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
echo "building bin/tailtab"
"$go" build -o bin/tailtab ./cmd/tailtab

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
