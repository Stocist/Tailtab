## Install

**Host** (macOS): download `tailtab-darwin-arm64` (Apple silicon) or `tailtab-darwin-amd64` (Intel), then:

```sh
chmod +x tailtab-darwin-arm64
xattr -d com.apple.quarantine tailtab-darwin-arm64   # the binary is not notarised yet
mkdir -p ~/.local/bin && mv tailtab-darwin-arm64 ~/.local/bin/tailtab
~/.local/bin/tailtab install --edge-id kejfineblfbjfolkgjkancapnpknomod --gecko-id tailtab@stocist.dev
```

Or build from source with `./scripts/build.sh`, which needs no quarantine step.

**Extension**:

- Zen / Firefox: install `tailtab-<version>.xpi` (signed by Mozilla for self-distribution) by opening it in the browser, or load `tailtab-firefox-<version>.zip` unpacked as a temporary add-on.
- Edge / Chrome: unzip `tailtab-chromium-<version>.zip` and load it unpacked from `edge://extensions` or `chrome://extensions` with developer mode on.

`SHA256SUMS` lists every asset. See the README for first login and the docs for how routing, exit nodes and accounts behave.
