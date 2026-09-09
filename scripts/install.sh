#!/bin/sh
# Installs the Tailtab native host on macOS or Linux and registers it with
# the browsers on this machine. No root needed; everything goes under $HOME.
#
#   curl -fsSL https://raw.githubusercontent.com/Stocist/Tailtab/main/scripts/install.sh | sh
#
# Environment:
#   TAILTAB_VERSION   release to install, e.g. 0.2.3 (default: latest)
#   TAILTAB_BIN_DIR   where the binary goes (default: ~/.local/bin)
set -eu

repo="Stocist/Tailtab"
edge_id="kejfineblfbjfolkgjkancapnpknomod"
gecko_id="tailtab@stocist.dev"

fail() { printf 'tailtab install: %s\n' "$*" >&2; exit 1; }

os="$(uname -s)"
case "$os" in
  Darwin) goos=darwin ;;
  Linux) goos=linux ;;
  *) fail "unsupported OS: $os (Windows: use scripts/install.ps1)" ;;
esac
arch="$(uname -m)"
case "$arch" in
  arm64|aarch64) goarch=arm64 ;;
  x86_64|amd64) goarch=amd64 ;;
  *) fail "unsupported architecture: $arch" ;;
esac

command -v curl >/dev/null || fail "curl is required"
if command -v sha256sum >/dev/null; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "sha256sum or shasum is required"
fi

if [ -n "${TAILTAB_VERSION:-}" ]; then
  base="https://github.com/$repo/releases/download/v${TAILTAB_VERSION#v}"
else
  base="https://github.com/$repo/releases/latest/download"
fi
asset="tailtab-$goos-$goarch"
bin_dir="${TAILTAB_BIN_DIR:-$HOME/.local/bin}"
dest="$bin_dir/tailtab"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf 'downloading %s/%s\n' "$base" "$asset"
curl -fsSL -o "$tmp/$asset" "$base/$asset" || fail "download failed (no release for $goos/$goarch?)"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || fail "could not download SHA256SUMS"

want="$(grep " $asset\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)"
[ -n "$want" ] || fail "SHA256SUMS has no entry for $asset"
got="$(sha "$tmp/$asset")"
[ "$got" = "$want" ] || fail "checksum mismatch for $asset: got $got, want $want"

mkdir -p "$bin_dir"
chmod +x "$tmp/$asset"
mv -f "$tmp/$asset" "$dest"
# curl does not set the quarantine flag, but clear it in case the file was
# downloaded by a browser first. The binary is not notarised yet.
[ "$goos" = darwin ] && xattr -d com.apple.quarantine "$dest" 2>/dev/null || true

printf 'installed %s\n' "$dest"
"$dest" install --edge-id "$edge_id" --gecko-id "$gecko_id"

cat <<MSG

Host installed. Now add the extension to your browser:

  Zen / Firefox:  open $base/tailtab-<version>.xpi in the browser
                  (signed by Mozilla; installs permanently and self-updates)
  Edge / Chrome:  unzip $base/tailtab-chromium-<version>.zip and load it
                  unpacked from edge://extensions or chrome://extensions
                  with developer mode on

Release page: https://github.com/$repo/releases/latest
MSG
