# Architecture

Tailtab is three pieces: a WebExtension, a native-messaging host written in Go, and a loopback proxy the host runs. The extension never touches the network itself; it only decides which requests the browser should send to the proxy, and it authenticates them.

## The host

`cmd/tailtab` is one binary with two jobs.

- **`tailtab install` / `uninstall`** write and remove the native-messaging manifests (`com.stocist.tailtab.json`) in the directories Edge, Zen/Firefox and Chrome read. The manifest records the absolute path of the binary. Zen reads Mozilla's directory and only that one.
- **Everything else** is host mode: the browser starts the binary with a couple of arguments of its own, and the host talks 4-byte little-endian length-prefixed JSON on stdin/stdout. Logging goes to stderr, which the browser captures; stdout carries the protocol and nothing else.

On `init` the host starts a `tsnet.Server` in a state directory of its own, `~/Library/Application Support/tailtab/<profile-uuid>/`, and watches Tailscale's IPN bus. Every change becomes a status event: node state, login URL, tailnet, device name and address, exit nodes, accounts, peers, health warnings, the proxy port and the proxy credential. The extension renders only what the host reports.

The UUID is generated once by the extension and kept in its `storage.local`. It is validated as a UUID before it becomes a path, and every profile gets its own directory: `tsnet` does not lock its state directory, and two nodes sharing one corrupt it silently. Deleting either the directory or the extension's storage orphans the node on the tailnet; log out first.

## The proxy

`internal/proxy` serves HTTP and SOCKS5 on one loopback listener, split by the first byte of each connection. Both dial through the node, by hostname, so MagicDNS resolution happens inside `tsnet` and never touches the machine's resolver for tailnet names.

Both protocols require the credential described in [security.md](security.md), and both run every destination through the same guard before dialling: tailnet names and addresses only, or, while an exit node is carrying traffic, anything that is not private address space.

## The extension

One source tree serves both browsers; `scripts/build.sh` copies it into `extension/dist/chromium` and `extension/dist/firefox` with the matching manifest and stamps a build id into the files.

- **Chromium (Edge)** gets a PAC script through `chrome.proxy.settings`. The PAC says `PROXY 127.0.0.1:<port>` for tailnet destinations and `DIRECT` for everything else. Chromium cannot authenticate SOCKS5, so the proxy is reached over HTTP and the 407 challenge is answered from `webRequest.onAuthRequired`.
- **Firefox (Zen)** decides per request in `proxy.onRequest`, returning a SOCKS5 proxy with `proxyDNS: true` and the credential inline, or `direct`.

`extension/rules.js` is the single source of the routing rules; the PAC is generated from it. The host has the same rules in Go, and `testdata/tailnet-hosts.json` and `testdata/exit-mode-hosts.json` are tables of decisions both implementations are tested against, so they cannot drift apart unnoticed.

## Restarts and workers

Chromium may stop the extension's service worker at any time, and it keeps the extension's proxy setting across worker restarts and extension reloads while the host process does not survive either. Three rules keep that honest:

- A starting worker drops any proxy setting it finds, because whatever it points at belongs to an earlier life; the setting is installed again once the new host is running.
- A proxy challenge that arrives before the new host has answered waits for it instead of declining; a challenge from a port this worker does not own is refused, and cancelled outright if it identifies itself as a Tailtab host, so no password dialog appears.
- A one-minute alarm reconnects a dead host even if the worker was asleep and lost its retry timer.

When the host dies, the proxy setting is cleared rather than left pointing at a dead port, so tailnet requests fail at once instead of hanging; ordinary browsing is unaffected either way because the PAC only ever routes tailnet hosts. While an exit node is selected the PAC has no direct fallback, which makes a dead host a kill switch until the extension reconnects.

Every build stamps an id into the worker and the popup. When they differ, which happens after a rebuild until the extension is reloaded, the popup says so instead of silently sending commands the worker does not know.
