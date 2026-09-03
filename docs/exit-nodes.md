# Exit nodes

If the tailnet offers an exit node, the popup shows an **Exit node** picker. Choosing one routes *this browser profile's entire* web traffic through that node, not just tailnet hosts, and choosing **None** puts the split tunnel back. Nothing outside the profile is affected: other browsers, other profiles and the rest of the machine carry on as before.

While an exit node is selected the routing rule flips. Everything public goes through the proxy and out via the exit node; what stays direct is loopback, link-local and private address space, which belong to the network you are sitting on rather than the exit node's. The extension and the host flip together, from the same status field, so neither sends traffic the other refuses.

Two behaviours worth knowing before you rely on it:

- **An offline exit node blocks browsing rather than falling back.** The popup says *Connected, exit node offline — browsing blocked*. The alternative is putting your traffic back on the local connection at the moment you were relying on it not being there. Pick **None** to browse normally again.
- **A dead host process blocks browsing too, while an exit node is selected.** The PAC has no direct fallback, so until the extension notices the host is gone and clears the setting, tailnet-bound and public traffic alike fail. It is the same kill-switch behaviour a VPN gives you, and it resolves itself when the extension reconnects.

Selection is stored by the exit node's stable node ID and survives restarts. An exit node that disappears from the tailnet entirely still shows as selected-but-offline rather than as "none".

## DNS

Names resolve through the exit node only when that node can proxy DNS, which current Tailscale exit nodes do. Against one that cannot, the traffic still leaves through the exit node but the name is looked up by this machine's own resolver, so the sites you visit are visible to your local DNS.

To check yours: with an exit node selected, `https://ifconfig.me` should show the exit node's public address, and a DNS-leak page such as `https://www.dnsleaktest.com` shows which resolver actually answered.

## Advertising one

Tailtab does not advertise its own node as an exit node. To offer one from a Linux machine on the tailnet:

```sh
sudo tailscale set --advertise-exit-node
```

then approve it under that machine's route settings in the admin console.
