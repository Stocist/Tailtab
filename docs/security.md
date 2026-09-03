# Security

## The proxy credential

The proxy listens on loopback, where every program on the machine can reach it. It therefore requires a credential: the username `tailtab` and a token of 32 random bytes that the host generates at startup and sends to the extension in every status event. Without it, any local process could open the port and browse the tailnet as this browser profile.

- **HTTP** (Edge, and anything using the listener as an HTTP proxy): the request must carry `Proxy-Authorization: Basic base64("tailtab:<token>")`, on CONNECT and on plain requests both. Anything else gets `407 Proxy Authentication Required`, before any destination check, so the 407 is not an oracle for the guard. Credentials are compared in constant time. The header is stripped before a request is forwarded.
- **SOCKS5** (Zen): username and password in the RFC 1929 handshake. A client that offers "no authentication" is refused. The handshake has a 30-second deadline. Tailscale's `socks5` package compares the password with a plain string comparison, which is an upstream property.

The token changes every time the host process starts. The extension keeps it in memory and nowhere else, not `storage.local`, not `storage.session`, not the popup, not a log line, and drops it the moment the host disconnects. A restarted worker always meets a new host with a new port and a new token.

To check by hand while a browser is connected, take the port from the popup:

```sh
curl -x http://127.0.0.1:<port> http://server/                       # 407
curl -x http://127.0.0.1:<port> -U tailtab:<token> http://server/    # proxied
```

## The destination guard

The listener only dials what the tailnet can serve: single-label MagicDNS names, `*.ts.net`, this node's own MagicDNS suffix (validated as a real multi-label DNS name and matched only on a label boundary), addresses in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`, and addresses inside a subnet a peer routes for the tailnet (the approved primary routes reported by the node; a malformed route is dropped, never widened, and so is any route broader than /8 or /16 or overlapping loopback, link-local, multicast or the unspecified address). Loopback, link-local and multicast are refused before the routes are consulted, whatever they say. Everything else is refused with `403` (HTTP) or a SOCKS failure reply.

While an exit node is selected *and online*, the guard flips to allow any public destination and refuse loopback, link-local and private address space, which would otherwise be dialled on the exit node's own network. Routed subnets stay allowed in exit mode: they belong to the tailnet, not to the exit node's LAN. Selected but offline keeps the tailnet-only guard, so traffic is blocked rather than sent out of the machine directly while the browser believes it is behind the exit node.

## Extension permissions

- `nativeMessaging`, `proxy`, `storage`, `tabs`, `alarms`: the host, the proxy configuration, the profile ID, opening the login tab, and the reconnect heartbeat.
- On Edge only: `webRequest`, `webRequestAuthProvider` and `host_permissions: <all_urls>`. `onAuthRequired` is the only way to answer a proxy's 407 in Manifest V3 and needs all three. The listener answers a challenge only when it comes from a proxy at `127.0.0.1` on the port this host is using, returns nothing for every other challenge, and never reads or modifies request content. Zen needs none of this because Firefox passes SOCKS credentials directly.

## Logging

`tsnet` uploads its logs to `log.tailscale.com` and there is no supported way to turn that off: the uploader is built unconditionally and neither documented off-switch applies to `tsnet`. Tailtab does not work around it. The host's own logging goes to stderr and never includes the proxy credential.

## Windows without a host

While the browser's worker has no host, after a crash or at worker start, the proxy setting is parked on a dead port rather than cleared, so a tailnet name is never resolved by the public resolver in that window. Only a disconnect the user asks for clears the setting.

## Known gaps

- A name that passes the guard but MagicDNS does not know still falls back to the system resolver inside `tsnet`'s dialer, so a `*.ts.net` name belonging to someone else's tailnet is a narrow remaining egress path for an *authenticated* caller. Closing it fully needs `UserDialPlan`.
- The MagicDNS suffix learned from the control plane is validated, but a hostile control server (Headscale or a compromised tailnet, not `login.tailscale.com`) could still report a two-label public domain such as `co.uk` and widen the rule to that one domain.
- Deleting an account is not offered: Tailscale's `DeleteProfile` reports success for an id that does not exist, so the UI could not tell a no-op from a deletion.
