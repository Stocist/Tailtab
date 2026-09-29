#!/usr/bin/env bash
# Each target receives only its compatible manifest as manifest.json.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go="${GO:-go}"
# Shared TAILTAB_VERSION overrides keep release versions synchronized across the host, extension files, and manifests.
ldflags="$("$root/scripts/ldflags.sh")"
commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo "building ${OUT:-bin/tailtab} ($(echo "$ldflags" | sed -E "s/.*longStamp=([^ ]+).*/\\1/"))"
"$go" build -ldflags "$ldflags" -o "${OUT:-bin/tailtab}" ./cmd/tailtab

src="extension"
dist="$src/dist"
# Explicitly allowlist bundle files so tests and adjacent sources never reach a browser.
shared=(background.js rules.js popup.html popup.js options.html options.js)

# Chromium caches background workers across restarts, so changing the manifest version forces rebuilt unpacked extensions to start fresh.
if [ -n "${TAILTAB_VERSION:-}" ]; then
  build_id="$TAILTAB_VERSION"
  ext_version="$TAILTAB_VERSION"
else
  build_id="${commit}$(git -C "$root" diff --quiet 2>/dev/null || echo -dirty)"
  ext_version="0.1.$(git -C "$root" rev-list --count HEAD 2>/dev/null || echo 0)"
fi

rm -rf "$dist"
for target in chromium firefox chromium-store firefox-listed; do
  out="$dist/$target"
  mkdir -p "$out/icons"
  for f in "${shared[@]}"; do
    sed -e "s/__TAILTAB_BUILD__/${build_id}/g" "$src/$f" > "$out/$f"
  done
  cp -R "$src"/icons/. "$out/icons/"
done

sed -e "s/\"version\": \"[0-9.]*\"/\"version\": \"${ext_version}\"/" "$src/manifest.chromium.json" > "$dist/chromium/manifest.json"
sed -e "s/\"version\": \"[0-9.]*\"/\"version\": \"${ext_version}\"/" "$src/manifest.firefox.json" > "$dist/firefox/manifest.json"
# The Chrome Web Store refuses a key on first upload, and AMO refuses update_url on a listed add-on.
sed -e '/"key":/d' "$dist/chromium/manifest.json" > "$dist/chromium-store/manifest.json"
# AMO shares version numbers between the listed and self-hosted builds, so the listed one takes a fourth part.
sed -e '/"update_url":/d' -e "s/\"version\": \"${ext_version}\"/\"version\": \"${ext_version}.1\"/" "$dist/firefox/manifest.json" > "$dist/firefox-listed/manifest.json"

echo "built $dist/chromium and $dist/firefox, plus store packages in $dist/chromium-store and $dist/firefox-listed"
echo "run ./scripts/test.sh to check the host and the extension"
