// Command tailtab is the native-messaging host behind the tailtab browser
// extension. It gives one browser profile its own Tailscale node via tsnet and
// exposes it to the browser as a loopback HTTP/SOCKS5 proxy.
//
// With no arguments it runs as a native-messaging host, speaking the framed
// JSON protocol in internal/nm over stdin and stdout. stdout is the protocol
// channel and nothing else may ever be written there; all output, including
// the output of the install and uninstall subcommands, goes to stderr.
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
  tailtab uninstall          remove the native-messaging manifests
`

func main() {
	// stdout is the native-messaging wire. Every log line goes to stderr.
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetOutput(os.Stderr)
	log.SetPrefix("tailtab: ")

	args := os.Args[1:]
	if len(args) == 0 {
		// The browser launches the host with no arguments; the manifest format
		// has no way to pass any.
		runHost()
		return
	}
	var err error
	switch args[0] {
	case "install":
		err = runInstall(args[1:])
	case "uninstall":
		err = runUninstall(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
	default:
		fmt.Fprintf(os.Stderr, "tailtab: unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating your home directory: %w", err)
	}
	exe, err := install.ExePath()
	if err != nil {
		return err
	}
	written, err := install.Install(install.Options{
		Home:    home,
		ExePath: exe,
		EdgeID:  *edgeID,
		GeckoID: *geckoID,
	})
	for _, p := range written {
		fmt.Fprintf(os.Stderr, "wrote %s\n", p)
	}
	if err != nil {
		return err
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
