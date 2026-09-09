## Install

**One command** (macOS / Linux):

```sh
curl -fsSL https://raw.githubusercontent.com/Stocist/Tailtab/main/scripts/install.sh | sh
```

**One command** (Windows, PowerShell):

```powershell
irm https://raw.githubusercontent.com/Stocist/Tailtab/main/scripts/install.ps1 | iex
```

Both verify the download against `SHA256SUMS` and run `tailtab install`. Manual steps follow if you prefer them.

**Host** (macOS): download `tailtab-darwin-arm64` (Apple silicon) or `tailtab-darwin-amd64` (Intel), then:

```sh
chmod +x tailtab-darwin-arm64
xattr -d com.apple.quarantine tailtab-darwin-arm64   # the binary is not notarised yet
mkdir -p ~/.local/bin && mv tailtab-darwin-arm64 ~/.local/bin/tailtab
~/.local/bin/tailtab install --edge-id kejfineblfbjfolkgjkancapnpknomod --gecko-id tailtab@stocist.dev
```

Or build from source with `./scripts/build.sh`, which needs no quarantine step.

**Host** (Linux): download `tailtab-linux-amd64` or `tailtab-linux-arm64`, `chmod +x` it, put it somewhere permanent such as `~/.local/bin/tailtab`, and run the same `install` command. Manifests go to `~/.config/microsoft-edge/`, `~/.mozilla/native-messaging-hosts/` and, when present, Chrome, Chromium and Brave.

**Host** (Windows): download `tailtab-windows-amd64.exe`, put it somewhere permanent such as `%LOCALAPPDATA%\tailtab\tailtab.exe`, and run the same `install` command from a terminal. It writes the manifests next to itself and registers them under `HKCU\Software\<browser>\NativeMessagingHosts`. Linux and Windows hosts pass the same end-to-end smoke test in CI as macOS; reports welcome.

**Extension**:

- Zen / Firefox: install `tailtab-<version>.xpi` (signed by Mozilla for self-distribution) by opening it in the browser. It updates itself from later releases through `updates.json`. Or load `tailtab-firefox-<version>.zip` unpacked as a temporary add-on.
- Edge / Chrome: unzip `tailtab-chromium-<version>.zip` and load it unpacked from `edge://extensions` or `chrome://extensions` with developer mode on.

`SHA256SUMS` lists every asset. See the README for first login and the docs for how routing, exit nodes and accounts behave.
