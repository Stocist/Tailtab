// Command tailtab runs the native-messaging host for the tailtab extension.
//
// Browser-supplied arguments default to host mode. Stdout is reserved for the
// framed JSON protocol; all other output goes to stderr.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Stocist/tailtab/internal/install"
)

const usage = `tailtab is the native host for the tailtab browser extension.

Usage:
  tailtab                    run as a native-messaging host (started by the browser)
  tailtab install --edge-id <chromium-extension-id> --gecko-id <addon-id>
                             register the native-messaging manifests
  tailtab install --chrome-flatpak --edge-id <chromium-extension-id> --extension-dir <directory>
                             copy the host and built extension into Chrome Flatpak (Linux)
  tailtab uninstall          remove the native-messaging manifests

Anything else on the command line means the browser started us, and we run as a
native-messaging host.
`

// Firefox-family browsers pass manifest and add-on arguments; Chromium-family
// browsers pass origin and window arguments. Only recognized commands leave
// host mode.
const (
	modeHost      = "host"
	modeInstall   = "install"
	modeUninstall = "uninstall"
	modeHelp      = "help"
)

func mode(args []string) string {
	if len(args) == 0 {
		return modeHost
	}
	switch args[0] {
	case "install":
		return modeInstall
	case "uninstall":
		return modeUninstall
	case "help", "-h", "--help":
		return modeHelp
	default:
		return modeHost
	}
}

func main() {
	// Stdout is reserved for native-messaging frames.
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetOutput(os.Stderr)
	log.SetPrefix("tailtab: ")

	args := os.Args[1:]
	var err error
	switch mode(args) {
	case modeInstall:
		err = runInstall(args[1:])
	case modeUninstall:
		err = runUninstall(args[1:])
	case modeHelp:
		fmt.Fprint(os.Stderr, usage)
	default:
		if len(args) > 0 {
			log.Printf("started with %d argument(s) from the browser: %q", len(args), args)
		}
		runHost()
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tailtab: %v\n", err)
		os.Exit(1)
	}
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	edgeID := fs.String("edge-id", "", "Microsoft Edge extension ID (32 characters, a-p)")
	geckoID := fs.String("gecko-id", "", "Zen/Firefox add-on ID, e.g. tailtab@stocist.dev")
	chromeFlatpak := fs.Bool("chrome-flatpak", false, "install only inside Chrome Flatpak on Linux; close Chrome before upgrading")
	extensionDir := fs.String("extension-dir", "", "built Chromium extension directory for --chrome-flatpak")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*chromeFlatpak && *extensionDir != "" {
		return fmt.Errorf("--extension-dir requires --chrome-flatpak")
	}
	if *chromeFlatpak && (*extensionDir == "" || fs.NArg() != 0) {
		return fmt.Errorf("--chrome-flatpak requires --extension-dir <built Chromium directory> and no positional arguments")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating your home directory: %w", err)
	}
	exe, err := install.ExePath()
	if err != nil {
		return err
	}
	opts := install.Options{
		Home:    home,
		ExePath: exe,
		EdgeID:  *edgeID,
		GeckoID: *geckoID,
	}
	var written []string
	if *chromeFlatpak {
		written, err = install.InstallChromeFlatpak(opts, *extensionDir)
	} else {
		written, err = install.Install(opts)
	}
	for _, p := range written {
		fmt.Fprintf(os.Stderr, "wrote %s\n", p)
	}
	if err != nil {
		return err
	}
	if *chromeFlatpak {
		fmt.Fprintln(os.Stderr, "Chrome Flatpak: load the copied extension directory above with Load unpacked at chrome://extensions.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "native host %q points at %s\n", install.HostName, exe)
	return nil
}

func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating your home directory: %w", err)
	}
	removed, err := install.Uninstall(home)
	for _, p := range removed {
		fmt.Fprintf(os.Stderr, "removed %s\n", p)
	}
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to remove")
	}
	return nil
}
