# tailtab

tailtab gives one browser profile its own Tailscale node. A WebExtension talks
over native messaging to a Go host that embeds `tsnet`; the host runs a
SOCKS5/HTTP proxy on loopback, and the extension sends only tailnet-bound
traffic there. The listener refuses anything else on its own account, so it is
a tailnet proxy rather than a general forward proxy. No system-wide Tailscale, no root, and two browser profiles can
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
browsing keeps working if the host process dies.

## Logging

`tsnet` uploads its logs to `log.tailscale.com` and there is no supported way
to turn that off — the uploader is built unconditionally, and neither
documented off-switch applies to `tsnet`. That is inherent to the library, and
tailtab does not work around it. The host's own logging goes to stderr, which
the browser captures: stdout carries the native-messaging protocol and nothing
else.

## Phase-0 limits

- macOS only; Zen and Edge only.
- No proxy authentication. The proxy binds a random port on 127.0.0.1 and takes
  any connection from this machine — Chromium cannot do SOCKS5 authentication
  at all, so loopback is the boundary. What a caller can reach through it is
  limited instead: the listener only dials MagicDNS names, `*.ts.net`, and
  addresses in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`, and refuses everything
  else with 403 (HTTP) or a SOCKS failure reply. A tailnet with a custom
  MagicDNS domain is not covered by that list and would be refused.
- No exit nodes, no peer list, no store packaging, placeholder icons.
- A temporary add-on in Zen has to be re-loaded after every restart.
- Firefox private windows are only covered if you enable *Run in Private
  Windows*; the popup says so when you have not.

## Development

```sh
go build ./... && go vet ./... && go test ./...
node scripts/test-extension.js
```

`scripts/test-extension.js` runs `extension/background.js` under a stubbed
Chromium API with fake timers, so the proxy-configuration lifecycle, the
reconnect backoff and the split-tunnel rules are testable without a browser.

The host can be driven by hand: it reads 4-byte little-endian length-prefixed
JSON on stdin and writes the same framing on stdout.
