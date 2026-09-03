# Architecture

Tailtab is three pieces: a WebExtension, a native-messaging host written in Go, and a loopback proxy the host runs. The extension never touches the network itself; it only decides which requests the browser should send to the proxy, and it authenticates them.

## The host

`cmd/tailtab` is one binary with two jobs.

- **`tailtab install` / `uninstall`** write and remove the native-messaging manifests (`com.stocist.tailtab.json`) where each browser looks for them. On macOS that is `~/Library/Application Support/<browser>/NativeMessagingHosts/`; on Linux `~/.config/<browser>/NativeMessagingHosts/` and `~/.mozilla/native-messaging-hosts/`; on Windows the two manifests (one per browser family) live in `%LOCALAPPDATA%\tailtab\` and `HKCU\Software\<vendor>\NativeMessagingHosts\com.stocist.tailtab` points at them. Edge and Zen/Firefox directories are created when missing; Chrome, Chromium and Brave are registered only if already installed. The manifest records the absolute path of the binary. Zen reads Mozilla's directory and only that one.
- **Everything else** is host mode: the browser starts the binary with a couple of arguments of its own, and the host talks 4-byte little-endian length-prefixed JSON on stdin/stdout. Logging goes to stderr, which the browser captures; stdout carries the protocol and nothing else.

On `init` the host starts a `tsnet.Server` in a state directory of its own under the user's config directory (`~/Library/Application Support/tailtab/<profile-uuid>/` on macOS, `~/.config/tailtab/<profile-uuid>/` on Linux, `%AppData%\tailtab\<profile-uuid>\` on Windows), and watches Tailscale's IPN bus. Every change becomes a status event: node state, login URL, tailnet, device name and address, exit nodes, accounts, peers, health warnings, the proxy port and the proxy credential. The extension renders only what the host reports.

The UUID is generated once by the extension and kept in its `storage.local`. It is validated as a UUID before it becomes a path, and every profile gets its own directory: `tsnet` does not lock its state directory, and two nodes sharing one corrupt it silently. Deleting either the directory or the extension's storage orphans the node on the tailnet; log out first.

## The proxy

`internal/proxy` serves HTTP and SOCKS5 on one loopback listener, split by the first byte of each connection. Both dial through the node, by hostname, so MagicDNS resolution happens inside `tsnet` and never touches the machine's resolver for tailnet names.

Both protocols require the credential described in [security.md](security.md), and both run every destination through the same guard before dialling: tailnet names and addresses only, or, while an exit node is carrying traffic, anything that is not private address space.

## The extension

One source tree serves both browsers; `scripts/build.sh` copies it into `extension/dist/chromium` and `extension/dist/firefox` with the matching manifest and stamps a build id into the files.

- **Chromium (Edge)** gets a PAC script through `chrome.proxy.settings`. The PAC says `PROXY 127.0.0.1:<port>` for tailnet destinations and `DIRECT` for everything else. Chromium cannot authenticate SOCKS5, so the proxy is reached over HTTP and the 407 challenge is answered from `webRequest.onAuthRequired`.
- **Firefox (Zen)** decides per request in `proxy.onRequest`, returning a SOCKS5 proxy with `proxyDNS: true` and the credential inline, or `direct`.

**Subnet routes.** The node accepts routes advertised by peers (`RouteAll`), and the status event carries every approved primary route as a CIDR. Both rules treat an address inside one as a tailnet destination: in the split tunnel it is proxied instead of going direct, and in exit mode it keeps going through the tailnet rather than being refused as private address space, which is what `tailscaled` does too. The PAC embeds the list, Firefox reads it per request, and the host's guard applies the same test before dialling. Routes match addresses only; a LAN name such as `nas.lan` reaches the tailnet only if the tailnet's DNS resolves it, which the browser cannot know in advance. Not every approved route is honoured: anything broader than /8 (IPv4) or /16 (IPv6), and anything overlapping loopback, link-local, multicast or the unspecified address, is dropped by the node and ignored by both rules, so a hostile route cannot turn the proxy on the user's own machine.

`extension/rules.js` is the single source of the routing rules; the PAC is generated from it. The host has the same rules in Go, and `testdata/tailnet-hosts.json` and `testdata/exit-mode-hosts.json` are tables of decisions both implementations are tested against, so they cannot drift apart unnoticed.

## Restarts and workers

Chromium may stop the extension's service worker at any time, and it keeps the extension's proxy setting across worker restarts and extension reloads while the host process does not survive either. Three rules keep that honest:

- A starting worker *parks* any proxy setting it finds: the same rules, pointed at a port nothing listens on. Whatever the old setting pointed at belongs to an earlier life, but clearing it would send tailnet names to the public resolver until the new host is up; parked, they fail at once instead. The real setting is installed again once the new host is running.
- A proxy challenge that arrives before the new host has answered waits for it instead of declining; a challenge from a port this worker does not own is refused, and cancelled outright if it identifies itself as a Tailtab host, so no password dialog appears.
- A one-minute alarm reconnects a dead host even if the worker was asleep and lost its retry timer.

When the host dies, the proxy setting is parked the same way rather than left pointing at a dead port, so tailnet requests fail at once instead of hanging and no tailnet name goes to the public resolver; ordinary browsing is unaffected because the PAC only ever routes tailnet hosts. Only a disconnect the user asked for clears the setting. While an exit node is selected the PAC has no direct fallback, which makes a dead host a kill switch until the extension reconnects.

Every build stamps an id into the worker and the popup. When they differ, which happens after a rebuild until the extension is reloaded, the popup says so instead of silently sending commands the worker does not know.
