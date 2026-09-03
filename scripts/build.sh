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
# The version stamp lives in scripts/ldflags.sh so the release workflow builds
# the same thing. TAILTAB_VERSION (a release tag like 0.1.0) overrides the
# commit-based build id everywhere: the host, the extension files and the
# manifest version.
ldflags="$("$root/scripts/ldflags.sh")"
commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo "building ${OUT:-bin/tailtab} ($(echo "$ldflags" | sed -E "s/.*longStamp=([^ ]+).*/\\1/"))"
"$go" build -ldflags "$ldflags" -o "${OUT:-bin/tailtab}" ./cmd/tailtab

src="extension"
dist="$src/dist"
# Copied by name, so extension/test/ and anything else alongside the sources
# never reaches a browser.
shared=(background.js rules.js popup.html popup.js options.html options.js)

# Every build gets an id (the commit, plus -dirty when the tree has edits) and
# a manifest version that grows with the commit count. Chromium keeps an
# extension's background worker cached across browser restarts; a changed
# manifest version makes it treat the reload as an update and start fresh, and
# the popup compares its own id with the worker's to say when they differ.
if [ -n "${TAILTAB_VERSION:-}" ]; then
  build_id="$TAILTAB_VERSION"
  ext_version="$TAILTAB_VERSION"
else
  build_id="${commit}$(git -C "$root" diff --quiet 2>/dev/null || echo -dirty)"
  ext_version="0.1.$(git -C "$root" rev-list --count HEAD 2>/dev/null || echo 0)"
fi

rm -rf "$dist"
for target in chromium firefox; do
  out="$dist/$target"
  mkdir -p "$out/icons"
  for f in "${shared[@]}"; do
    sed -e "s/__TAILTAB_BUILD__/${build_id}/g" "$src/$f" > "$out/$f"
  done
  cp -R "$src"/icons/. "$out/icons/"
done

# The manifest version is what makes Chromium notice a rebuilt unpacked
# extension on the next browser start (see build_id above).
sed -e "s/\"version\": \"[0-9.]*\"/\"version\": \"${ext_version}\"/" "$src/manifest.chromium.json" > "$dist/chromium/manifest.json"
sed -e "s/\"version\": \"[0-9.]*\"/\"version\": \"${ext_version}\"/" "$src/manifest.firefox.json" > "$dist/firefox/manifest.json"

echo "built $dist/chromium and $dist/firefox"
echo "run ./scripts/test.sh to check the host and the extension"
