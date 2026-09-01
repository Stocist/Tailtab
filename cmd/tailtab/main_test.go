package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Argument lists browsers actually hand a native-messaging host. None of them
// is a subcommand, and every one of them must land in host mode.
var browserArgs = map[string][]string{
	"no arguments": {},
	"firefox and zen": {
		"/Users/someone/Library/Application Support/Mozilla/NativeMessagingHosts/com.stocist.tailtab.json",
		"tailtab@stocist.dev",
	},
	"chromium and edge": {
		"chrome-extension://kejfineblfbjfolkgjkancapnpknomod/",
	},
	"chromium with a parent window": {
		"chrome-extension://kejfineblfbjfolkgjkancapnpknomod/",
		"--parent-window=0",
	},
}

func TestModeSelection(t *testing.T) {
	for name, args := range browserArgs {
		if got := mode(args); got != modeHost {
			t.Errorf("%s: mode(%q) = %q, want %q", name, args, got, modeHost)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"install", "--edge-id", "x"}, modeInstall},
		{[]string{"uninstall"}, modeUninstall},
		{[]string{"help"}, modeHelp},
		{[]string{"-h"}, modeHelp},
		{[]string{"--help"}, modeHelp},
	} {
		if got := mode(tc.args); got != tc.want {
			t.Errorf("mode(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestBrowserArgvEntersHostMode runs the real binary the way a browser does.
// This is the end-to-end form of the bug: the host printed usage and exited 2
// the moment Zen launched it, which the extension saw as a host that would not
// stay up.
func TestBrowserArgvEntersHostMode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	exe := filepath.Join(t.TempDir(), "tailtab")
	if out, err := exec.Command(goBin, "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("building the host: %v\n%s", err, out)
	}

	for name, args := range browserArgs {
		t.Run(name, func(t *testing.T) {
			// One framed status command, then EOF. A status before init needs
			// no node and touches no network.
			body := []byte(`{"cmd":"status"}`)
			var stdin bytes.Buffer
			var hdr [4]byte
			binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
			stdin.Write(hdr[:])
			stdin.Write(body)

			cmd := exec.Command(exe, args...)
			cmd.Stdin = &stdin
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting the host: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("host exited with %v\nstderr:\n%s", err, stderr.String())
				}
			case <-time.After(30 * time.Second):
				cmd.Process.Kill()
				t.Fatal("the host did not exit when stdin closed")
			}

			// Host mode answered on the wire, so it did not print usage and quit.
			out := stdout.Bytes()
			if len(out) < 4 {
				t.Fatalf("no framed reply on stdout; stderr:\n%s", stderr.String())
			}
			n := int(binary.LittleEndian.Uint32(out[:4]))
			if len(out) != 4+n {
				t.Fatalf("stdout holds %d bytes for a %d byte message; stdout must carry the protocol and nothing else", len(out), n)
			}
			var ev struct {
				Event string `json:"event"`
				State string `json:"state"`
			}
			if err := json.Unmarshal(out[4:], &ev); err != nil {
				t.Fatalf("reply is not JSON: %v (%q)", err, out[4:])
			}
			if ev.Event != "status" {
				t.Errorf("reply = %+v, want a status event", ev)
			}
			if len(args) > 0 && !bytes.Contains(stderr.Bytes(), []byte("argument(s) from the browser")) {
				t.Errorf("the arguments were not logged to stderr; got:\n%s", stderr.String())
			}
		})
	}
}
