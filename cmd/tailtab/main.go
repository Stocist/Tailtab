// Command tailtab is the native-messaging host behind the tailtab browser
// extension. It gives one browser profile its own Tailscale node via tsnet and
// exposes it to the browser as a loopback HTTP/SOCKS5 proxy.
//
// With no arguments it runs as a native-messaging host, speaking the framed
// JSON protocol in internal/nm over stdin and stdout. stdout is the protocol
// channel and nothing else may ever be written there; all logging goes to
// stderr.
package main

import (
	"fmt"
	"log"
	"os"
)

const usage = `tailtab is the native host for the tailtab browser extension.

Usage:
  tailtab                    run as a native-messaging host (started by the browser)
  tailtab install [flags]    register the native-messaging manifests
  tailtab uninstall          remove the native-messaging manifests
`

func main() {
	// stdout is the native-messaging wire. Every log line goes to stderr.
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetOutput(os.Stderr)
	log.SetPrefix("tailtab: ")

	args := os.Args[1:]
	if len(args) == 0 {
		runHost()
		return
	}
	switch args[0] {
	case "install", "uninstall":
		fmt.Fprintf(os.Stderr, "tailtab: %q is not implemented yet\n", args[0])
		os.Exit(2)
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
	default:
		fmt.Fprintf(os.Stderr, "tailtab: unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
}
