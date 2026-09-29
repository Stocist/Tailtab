# Privacy policy

Tailtab does not collect, sell or share your data. It has no analytics, no telemetry of its own and no server run by its author.

## What stays on your computer

- A random profile ID, the routing rules of your tailnet and, if you set one, the address of your coordination server. The extension keeps these in the browser's extension storage.
- Your Tailscale node key and state. The native host keeps these in your user profile, one directory per browser profile.
- The proxy credential. The host generates it at every start and the extension holds it in memory only.

## What leaves your computer

- Traffic to your tailnet, and all web traffic of the profile while an exit node is selected, goes through the Tailscale network from the native host. Tailtab does not read, log or store the contents.
- The native host talks to the Tailscale coordination server, or the one you configured, to log in and find your machines.
- The Tailscale library inside the host uploads its own diagnostic logs to Tailscale. Tailtab cannot switch this off. See the [Tailscale privacy policy](https://tailscale.com/privacy-policy).
- The popup loads your account's avatar over HTTPS from the address your identity provider gives.

## Permissions

[docs/security.md](security.md#extension-permissions) lists every browser permission and why it is needed.

## Contact

Open an issue at https://github.com/Stocist/Tailtab/issues.

Tailtab is an independent project and is not affiliated with Tailscale Inc.
