# tailtab

tailtab gives one browser profile its own Tailscale node. A WebExtension talks
over native messaging to a Go host that embeds `tsnet`; the host runs a
SOCKS5/HTTP proxy on loopback, and the extension sends only tailnet-bound
traffic there. The listener refuses anything else on its own account, so it is
a tailnet proxy rather than a general forward proxy, and it takes a credential
that only this extension has, so no other program on the machine can use it. No system-wide Tailscale, no root, and two browser profiles can
sit on two different tailnets at once.

Phase 0 targets macOS with Zen (Firefox 154-based) and Microsoft Edge.

## Build

```sh
./scripts/build.sh
```

That produces `bin/tailtab`, `extension/dist/chromium/` (Edge) and
`extension/dist/firefox/` (Zen).

## Install the native host

The manifest records the absolute path of `bin/tailtab`, so re-run this if you
move the repository.

```sh
bin/tailtab install \
  --edge-id kejfineblfbjfolkgjkancapnpknomod \
  --gecko-id tailtab@stocist.dev
```

It writes `com.stocist.tailtab.json` to:

- `~/Library/Application Support/Microsoft Edge/NativeMessagingHosts/`
- `~/Library/Application Support/Mozilla/NativeMessagingHosts/` — Zen reads
  Mozilla's directory and only that one, so this covers Zen and Firefox both.
  The directory is created if it does not exist.
- `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/`, if you
  have Chrome.

`bin/tailtab uninstall` removes those three files and nothing else.

The Edge extension ID above is fixed by the `key` in `manifest.chromium.json`,
so the unpacked extension keeps the same ID on every machine and path.

## Load the extension

- **Zen**: `about:debugging#/runtime/this-firefox` → *Load Temporary Add-on* →
  pick `extension/dist/firefox/manifest.json`. A temporary add-on is removed
  when Zen restarts. To cover private windows, turn on *Run in Private Windows*
  for tailtab in `about:addons`.
- **Edge**: `edge://extensions` → turn on *Developer mode* → *Load unpacked* →
  pick `extension/dist/chromium/`.

## First login

Open the tailtab popup. It shows `NeedsLogin` with a **Log in** button, which
opens the Tailscale auth URL in a tab. Approve the node there; the popup then
shows *Connected* with the tailnet, machine name, node address, and the proxy
port. `http://wiki/` and `http://wiki.tail4d5e6f.ts.net/` go through the
tailnet; `https://github.com` goes direct.

Each browser profile gets its own node, named `<your-mac>-tailtab-zen` or
`<your-mac>-tailtab-edge`.

## Where state lives

- Node state (including the node key): `~/Library/Application Support/tailtab/<profile-uuid>/`.
  The UUID is generated once by the extension and kept in its `storage.local`.
  Deleting either one orphans the node; log out first if you want it gone from
  the tailnet.
- The profile ID is validated as a UUID before it is used as a path, and each
  profile gets its own directory: `tsnet` does not lock its state directory and
  two nodes sharing one corrupt it silently.

## Split tunnel

`extension/rules.js` is the single source of truth, used directly by Firefox's
`proxy.onRequest` and compiled into a PAC script for Edge. A host is proxied
when it is a single-label MagicDNS name (`wiki`), ends in `.ts.net` or your
tailnet's MagicDNS suffix, or is an address in `100.64.0.0/10` or
`fd7a:115c:a1e0::/48`. Everything else is DIRECT, which is why ordinary
browsing keeps working if the host process dies. The same rule exists twice —
in `extension/rules.js` and as the host's own guard in `internal/proxy` — and
`testdata/tailnet-hosts.json` is the table both are tested against, so the two
cannot drift apart unnoticed.

When the host process dies, Edge's PAC script is cleared rather than left in
place. Both are safe for ordinary browsing, but a PAC pointing at a dead port
makes tailnet requests hang until the browser gives up, where no PAC makes them
fail at once. It is reinstalled as soon as the host is back on its new port.

## Authentication

The proxy listens on loopback, where every program on the machine can reach it.
It therefore requires a credential: the username `tailtab` and a token of 32
random bytes that the host generates at startup and sends to the extension in
every status event. Without it, any local process could open the port and browse
the tailnet as this browser profile.

- **HTTP** (Edge, and anything using the listener as an HTTP proxy): the request
  must carry `Proxy-Authorization: Basic base64("tailtab:<token>")`, on CONNECT
  and on plain requests both. Anything else gets `407 Proxy Authentication
  Required`. The header is stripped before a request is forwarded, so tailnet
  services never see it.
- **SOCKS5** (Zen): username and password in the RFC 1929 handshake. A client
  that offers "no authentication" is refused.

The token changes every time the host process starts, so nothing on disk is
worth stealing later. The extension keeps it in memory and nowhere else — not
`storage.local`, not `storage.session`, not the popup, not a log line — and
drops it the moment the host disconnects. There is nothing worth saving: the
host is a child of the extension's background page, so a restarted background
page always meets a new host with a new port and a new token.

Edge reaches the proxy over HTTP rather than SOCKS5 for exactly this reason:
Chromium has never implemented SOCKS5 authentication, so the PAC says `PROXY`
and the extension answers the 407 challenge from
`chrome.webRequest.onAuthRequired`.

To check it by hand while a browser is connected, take the port from the popup:

```sh
curl -x http://127.0.0.1:<port> http://server/          # 407
curl -x http://127.0.0.1:<port> -U tailtab:<token> http://server/
```

The first is the point: another program on your machine cannot ride the
profile's tailnet identity.

## Permissions

- `nativeMessaging`, `proxy`, `storage`, `tabs` — the native host, the proxy
  configuration, the profile ID and last status, and opening the login tab.
- `webRequest`, `webRequestAuthProvider` and `host_permissions: <all_urls>`, on
  Edge only. `onAuthRequired` is the only way to answer the proxy's 407 in MV3,
  and it needs all three. This is a real widening of what the extension may see,
  so: the listener registered under it answers a challenge only when it comes
  from a proxy at `127.0.0.1` on the port this host is using, returns nothing at
  all for every other challenge, and never reads or modifies request content.
  `webRequestAuthProvider` carries no install-time permission warning of its own.
  Zen needs none of this — Firefox passes SOCKS credentials directly — and its
  manifest does not ask for them.

## Logging

`tsnet` uploads its logs to `log.tailscale.com` and there is no supported way
to turn that off — the uploader is built unconditionally, and neither
documented off-switch applies to `tsnet`. That is inherent to the library, and
tailtab does not work around it. The host's own logging goes to stderr, which
the browser captures: stdout carries the native-messaging protocol and nothing
else.

## Limits

- macOS only; Zen and Edge only.
- The listener's destination guard is by name: it only dials MagicDNS names,
  `*.ts.net`, this node's own MagicDNS suffix, and addresses in `100.64.0.0/10`
  or `fd7a:115c:a1e0::/48`, refusing everything else with 403 (HTTP) or a SOCKS
  failure reply. A name that passes the guard but MagicDNS does not know still
  falls back to the system resolver inside `UserDial`, so a `*.ts.net` name
  belonging to someone else's tailnet is a narrow remaining egress path for an
  authenticated caller.
- No exit nodes, no peer list, no store packaging, placeholder icons.
- A temporary add-on in Zen has to be re-loaded after every restart.
- Firefox private windows are only covered if you enable *Run in Private
  Windows*; the popup says so when you have not.

## Development

```sh
./scripts/test.sh
```

That runs `go vet`, `go test ./...`, and the extension's tests.
`extension/test/background.test.js` loads `background.js` unmodified into a
stubbed Chromium MV3 environment with fake timers, so the proxy-configuration
lifecycle, the reconnect backoff and the split-tunnel rules are all testable
without a browser. `build.sh` copies extension sources by name, so the test
directory never ends up in a dist.

The host can be driven by hand: it reads 4-byte little-endian length-prefixed
JSON on stdin and writes the same framing on stdout.
