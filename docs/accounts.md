# Accounts

A Tailtab node can hold several Tailscale accounts and switch between them without logging in again. This is Tailscale's own *login profiles* feature, exposed in the popup: each account keeps its own node key, its own tailnet and its own state, all inside the one state directory for the browser profile.

## In the popup

The header shows the active account, its display name (or login name) and its tailnet, with the account's picture when the identity provider supplies one. Clicking it opens the switcher:

- Every account the node holds, with the active one marked. Choosing another switches immediately; the popup says *Switching account…* until the host reports the new one active.
- **Add account…** starts a login for a new account. The Tailscale login page opens in a tab on its own; approve the node there and the popup comes back on the new account.

A node that has never completed a login has no accounts yet; the switcher then offers only *Add account…*.

## What a switch does

Tailscale resets the node's preferences when it switches profiles, and `tsnet` only applies Tailtab's own preferences once, at start. After every switch, and after every logout, Tailtab puts them back before the next login request: the device name and the "want running" flag. Without that, a re-login would register the node under the machine's operating-system hostname.

Everything belonging to the old account is cleared from the status the moment a switch starts: its tailnet, device name, address, peers, exit nodes and any pending login URL. What the new account has arrives from the bus as it comes up.

## Removing an account

Not offered from the popup. Tailscale's profile deletion reports success even for an id that does not exist, so the UI could not tell a no-op from a real deletion. To retire an account, log it out from the popup (which invalidates its key on the tailnet) and remove the machine in the admin console.

## A different coordination server

Tailtab talks to Tailscale's coordination server by default. A self-hosted one such as Headscale goes in the settings page (the **Settings** link in the popup footer, or the extension's options page).

The server is chosen **per browser profile, at the first login**, and pinned from then on in the node's state directory. The reason is a `tsnet` property: it rewrites the active account's preferences, coordination server included, on every start, so Tailtab has to pass the same server every time or an account would be silently repointed. Consequences:

- The setting applies to a browser profile that has not logged in yet. Change it before the first login, or in a fresh browser profile.
- Every account in one browser profile uses that profile's server. *Add account…* against a different server is refused with a message saying so; use another browser profile for it.
- If the setting differs from what the profile is pinned to, the popup says so in its warnings rather than applying it.
- The popup shows a **Control server** line whenever the active account is not on Tailscale's.

The URL is validated in the settings page and again by the host: http or https, a host, no credentials, no query or fragment.
