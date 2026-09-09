# Chrome Flatpak on Linux

Flatpak Chrome cannot see an arbitrary host binary or unpacked extension in your home directory. Tailtab's opt-in installer copies both into `com.google.Chrome`'s app directory and registers only that browser. It does not change sandbox permissions, use `flatpak-spawn --host`, or alter native browser installations.

## Install

Install and launch Chrome Flatpak once so `~/.var/app/com.google.Chrome/` exists, then fully quit Chrome, including background processes. Run these commands from the Tailtab repository on Linux:

```sh
CGO_ENABLED=0 ./scripts/build.sh
bin/tailtab install --chrome-flatpak --extension-dir extension/dist/chromium
```

Building requires Go 1.27 and Node 22. `CGO_ENABLED=0` avoids depending on the host distribution's C library inside the Flatpak runtime. The regular download installer remains native-browser-only; this command requires a host build that includes `--chrome-flatpak` and a built Chromium bundle, not `extension/` or the Firefox bundle.

The installer prints the resulting paths:

- Host: `~/.var/app/com.google.Chrome/data/tailtab/bin/tailtab`
- Extension: `~/.var/app/com.google.Chrome/data/tailtab/extension`
- Manifest: `~/.var/app/com.google.Chrome/config/google-chrome/NativeMessagingHosts/com.stocist.tailtab.json`

Reopen Chrome, visit `chrome://extensions`, enable **Developer mode**, and use **Load unpacked** to select the copied extension directory. Its ID must be `kejfineblfbjfolkgjkancapnpknomod`. Open the popup and connect normally.

The host runs inside Chrome's sandbox. It uses Flatpak's writable `XDG_CONFIG_HOME` for node state and the browser's shared network for its loopback proxy. No root privileges or Flatpak overrides are required. The installer deliberately ignores the host shell's `XDG_CONFIG_HOME`.

## Upgrade

Fully quit Chrome before rerunning the build and install commands. Reopen Chrome and reload the extension at `chrome://extensions` if necessary. The paths stay fixed, and files removed from the new extension bundle do not linger in the installed copy.

The installer stages the host, extension, and manifest before replacing them and rolls back if replacement fails. Chrome must be closed because the three paths cannot be swapped simultaneously. Browser preferences, extension storage, other native hosts, and node state under `~/.var/app/com.google.Chrome/config/tailtab/` are left alone.

A per-app lock prevents overlapping installs. If another install is running, wait and retry; the OS releases the lock when that process exits. The `.tailtab-install.lock` file is intentionally retained, and its presence alone does not mean an install is active.

Keep using the existing installed extension when upgrading; removing it through Chrome clears its extension storage. Native and Flatpak installations are separate, and the installer does not migrate node identities between them.

## Remove

Quit Chrome and remove only the installed artifacts:

```sh
app="$HOME/.var/app/com.google.Chrome"
rm -f "$app/config/google-chrome/NativeMessagingHosts/com.stocist.tailtab.json"
rm -f "$app/data/tailtab/bin/tailtab"
rm -rf "$app/data/tailtab/extension"
```

Then remove the unpacked extension from `chrome://extensions`. The commands retain node state under the app's `config/tailtab/`. The ordinary `tailtab uninstall` command still targets native browser manifests, not this Flatpak installation.
