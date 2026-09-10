package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFlatpakInstallFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--extension-dir", "bundle"},
		{"--chrome-flatpak"},
		{"--chrome-flatpak", "--extension-dir", "bundle", "extra"},
	} {
		if err := runInstall(args); err == nil || !strings.Contains(err.Error(), "--") {
			t.Fatalf("runInstall(%q) = %v; want a flag error", args, err)
		}
	}
}

func TestChromeFlatpakInstallCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a host binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	root := t.TempDir()
	exe := filepath.Join(root, "tailtab")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(goBin, "build", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building host: %v\n%s", err, out)
	}
	home := filepath.Join(root, "home with spaces")
	app := filepath.Join(home, ".var", "app", "com.google.Chrome")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(root, "built extension")
	if err := os.MkdirAll(extension, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "..", "extension")
	for _, name := range []string{"manifest.chromium.json", "background.js", "rules.js", "popup.html", "popup.js", "options.html", "options.js"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "manifest.chromium.json" {
			name = "manifest.json"
		}
		b = bytes.ReplaceAll(b, []byte("__TAILTAB_BUILD__"), []byte("flatpak-test"))
		if err := os.WriteFile(filepath.Join(extension, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.CopyFS(filepath.Join(extension, "icons"), os.DirFS(filepath.Join(src, "icons"))); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(root, "native config"))
	args := []string{"install", "--chrome-flatpak", "--edge-id", "kejfineblfbjfolkgjkancapnpknomod", "--extension-dir", extension}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if runtime.GOOS != "linux" {
		if err == nil || !strings.Contains(string(out), "only on Linux") {
			t.Fatalf("non-Linux installation = %v, %s", err, out)
		}
		entries, err := os.ReadDir(app)
		if err != nil || len(entries) != 0 {
			t.Fatalf("non-Linux installation wrote files: %v, %v", entries, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "chrome://extensions") {
		t.Fatalf("missing extension loading instructions: %s", out)
	}
	b, err := os.ReadFile(filepath.Join(app, "config", "google-chrome", "NativeMessagingHosts", "com.stocist.tailtab.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Path string }
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	installedExtension := filepath.Join(app, "data", "tailtab", "extension")
	args[len(args)-1] = installedExtension
	cmd = exec.CommandContext(ctx, manifest.Path, args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rerun from installed artifacts: %v\n%s", err, out)
	}
	// Exercise the copied executable without logging in or creating node state.
	body := []byte(`{"cmd":"status"}`)
	var frame bytes.Buffer
	if err := binary.Write(&frame, binary.LittleEndian, uint32(len(body))); err != nil {
		t.Fatal(err)
	}
	frame.Write(body)
	cmd = exec.CommandContext(ctx, manifest.Path)
	cmd.Env = append(env, "XDG_CONFIG_HOME="+filepath.Join(app, "config"))
	cmd.Stdin = &frame
	out, err = cmd.Output()
	if err != nil || len(out) < 4 || int(binary.LittleEndian.Uint32(out[:4])) != len(out)-4 {
		t.Fatalf("copied host native messaging: %v, %q", err, out)
	}
	var event struct{ Event string }
	if err := json.Unmarshal(out[4:], &event); err != nil || event.Event != "status" {
		t.Fatalf("copied host status: %+v, %v", event, err)
	}
}
