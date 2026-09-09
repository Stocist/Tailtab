package install

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const flatpakTestKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA8ulFItdPwEt5XDJoeh8A51zcIgZyF/rjMdg9xkONKioF1hPbfVE+6mzdD/zQit1aEXobp+G+5GZCBGSGr2qF7VxTxjY7mxdIakAjyuGQGTaWosTHV3Cwl6W64tVL9TdrLLJw2QvVfwUhDycEfBtKOyTeqjd5fEqCsEiAzML9ubGd7Zye3VgnGXpe+LO7tDZvNZKg8TyEH9HBjTCoZX5VgnpJJ6yPubeQRoAV3b7khCbY3mKOfXNY06LDyKmnhLTk22pCTRwA9Gr36J1MPqcFnjr1j9Z35yW3JdH8um4Y2Zn4+OYGQYVJYIXli9SeC/W5TJ55VSGz45HYjY2U4prlcQIDAQAB"

func flatpakFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{GOOS: "linux", Home: filepath.Join(root, "home"), ExePath: filepath.Join(root, "host"), EdgeID: "kejfineblfbjfolkgjkancapnpknomod"}
	app := filepath.Join(opts.Home, ".var", "app", "com.google.Chrome")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(root, "bundle")
	flatpakWrite(t, opts.ExePath, "host v1", 0o751)
	flatpakWrite(t, filepath.Join(extension, "manifest.json"), fmt.Sprintf(`{
		"manifest_version": 3, "name": "tailtab", "version": "0.1.0", "key": %q,
		"permissions": ["nativeMessaging"], "background": {"service_worker": "background.js"},
		"options_ui": {"page": "options.html"},
		"action": {"default_popup": "popup.html", "default_icon": {"16": "icons/icon.png"}},
		"icons": {"16": "icons/icon.png"}
	}`, flatpakTestKey), 0o644)
	for _, path := range []string{"background.js", "options.html", "popup.html", "icons/icon.png", "nested/asset.css"} {
		flatpakWrite(t, filepath.Join(extension, path), "original "+path, 0o644)
	}
	return opts, extension, app
}

func flatpakWrite(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func flatpakCheck(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s = %q, %v; want %q", path, b, err, want)
	}
}

func flatpakNoStage(t *testing.T, app string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(app, ".tailtab-install-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("staging leftovers = %v, %v", matches, err)
	}
}

func TestInstallChromeFlatpak(t *testing.T) {
	opts, extension, app := flatpakFixture(t)
	sourceInfo, err := os.Stat(opts.ExePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceDirInfo, err := os.Stat(extension)
	if err != nil {
		t.Fatal(err)
	}
	xdg := filepath.Join(filepath.Dir(opts.Home), "host-xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	untouched := []string{
		filepath.Join(opts.Home, ".config/google-chrome/NativeMessagingHosts", manifestFile),
		filepath.Join(xdg, "google-chrome/NativeMessagingHosts", manifestFile),
		filepath.Join(xdg, "microsoft-edge/NativeMessagingHosts", manifestFile),
		filepath.Join(xdg, "tailtab/0f8fad5b-d9cb-469f-a165-70867728950e/tailscaled.state"),
		filepath.Join(opts.Home, ".mozilla/native-messaging-hosts", manifestFile),
		filepath.Join(opts.Home, ".var/app/com.microsoft.Edge/config/microsoft-edge/NativeMessagingHosts", manifestFile),
		filepath.Join(opts.Home, ".var/app/org.chromium.Chromium/config/chromium/NativeMessagingHosts", manifestFile),
		filepath.Join(opts.Home, ".var/app/org.mozilla.firefox/.mozilla/native-messaging-hosts", manifestFile),
		filepath.Join(app, "config/google-chrome/Default/Preferences"),
		filepath.Join(app, "config/google-chrome/NativeMessagingHosts/com.example.other.json"),
		filepath.Join(app, "config/tailtab/0f8fad5b-d9cb-469f-a165-70867728950e/tailscaled.state"),
		filepath.Join(app, "data/tailtab/state/node.json"),
		filepath.Join(app, "data/tailtab/profiles/profile/state"),
		filepath.Join(app, "data/tailtab/bin/other"),
	}
	for _, path := range untouched {
		flatpakWrite(t, path, "preserve me", 0o600)
	}
	paths, err := InstallChromeFlatpak(opts, extension)
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(app, "data/tailtab/bin/tailtab")
	installed := filepath.Join(app, "data/tailtab/extension")
	nativeManifest := filepath.Join(app, "config/google-chrome/NativeMessagingHosts", manifestFile)
	if len(paths) != 3 || !slices.Contains(paths, host) || !slices.Contains(paths, installed) || !slices.Contains(paths, nativeManifest) {
		t.Fatalf("written paths = %v", paths)
	}
	m := decode(t, nativeManifest)
	if m.Name != HostName || m.Description != description || m.Type != "stdio" || m.Path != host ||
		!slices.Equal(m.AllowedOrigins, []string{"chrome-extension://" + opts.EdgeID + "/"}) || len(m.AllowedExtensions) != 0 {
		t.Fatalf("manifest = %+v", m)
	}
	flatpakCheck(t, host, "host v1")
	for _, path := range []string{"manifest.json", "background.js", "options.html", "popup.html", "icons/icon.png", "nested/asset.css"} {
		b, err := os.ReadFile(filepath.Join(extension, path))
		if err != nil {
			t.Fatal(err)
		}
		flatpakCheck(t, filepath.Join(installed, path), string(b))
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{host: 0o755, nativeManifest: 0o644, installed: 0o755, filepath.Join(installed, "background.js"): 0o644, filepath.Join(installed, "icons"): 0o755} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// The source directory was created with 0755 under the same umask.
			owner, allowed := mode&0o700, mode&sourceDirInfo.Mode().Perm()
			got := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
			if got&owner != owner || got&^allowed != 0 {
				t.Fatalf("permissions of %s = %o; require owner bits %o and no bits outside %o", path, got, owner, allowed)
			}
		}
	}
	sourceAfter, err := os.Stat(opts.ExePath)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter.Mode() != sourceInfo.Mode() {
		t.Fatalf("source mode changed from %v to %v", sourceInfo.Mode(), sourceAfter.Mode())
	}
	var oldHost *os.File
	if runtime.GOOS != "windows" {
		oldHost, err = os.Open(host)
		if err != nil {
			t.Fatal(err)
		}
		defer oldHost.Close()
	}
	flatpakWrite(t, opts.ExePath, "host v2", 0o755)
	flatpakWrite(t, filepath.Join(extension, "background.js"), "worker v2", 0o644)
	flatpakWrite(t, filepath.Join(installed, "stale.js"), "stale", 0o644)
	if err := os.Remove(filepath.Join(extension, "nested/asset.css")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallChromeFlatpak(opts, extension); err != nil {
		t.Fatal(err)
	}
	flatpakCheck(t, host, "host v2")
	flatpakCheck(t, filepath.Join(installed, "background.js"), "worker v2")
	for _, path := range []string{"stale.js", "nested/asset.css"} {
		if _, err := os.Stat(filepath.Join(installed, path)); !os.IsNotExist(err) {
			t.Fatalf("stale asset %s remains: %v", path, err)
		}
	}
	if oldHost != nil {
		b, err := io.ReadAll(oldHost)
		if err != nil || string(b) != "host v1" {
			t.Fatalf("previous executable inode was modified: %q, %v", b, err)
		}
	}
	opts.ExePath = host
	if _, err := InstallChromeFlatpak(opts, installed); err != nil {
		t.Fatalf("rerun from installed host and extension: %v", err)
	}
	flatpakCheck(t, host, "host v2")
	flatpakCheck(t, filepath.Join(installed, "background.js"), "worker v2")
	for _, path := range untouched {
		flatpakCheck(t, path, "preserve me")
	}
	flatpakNoStage(t, app)
}

func TestInstallChromeFlatpakStringIcon(t *testing.T) {
	opts, extension, _ := flatpakFixture(t)
	path := filepath.Join(extension, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["action"] = json.RawMessage(`{"default_icon":"icons/icon.png"}`)
	b, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	flatpakWrite(t, path, string(b), 0o644)
	if _, err := InstallChromeFlatpak(opts, extension); err != nil {
		t.Fatalf("valid string default_icon rejected: %v", err)
	}
}

func TestInstallChromeFlatpakRejectsInputs(t *testing.T) {
	changeManifest := func(field string, value any) func(*testing.T, *Options, *string) {
		return func(t *testing.T, _ *Options, dir *string) {
			path := filepath.Join(*dir, "manifest.json")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			m[field] = value
			b, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			flatpakWrite(t, path, string(b), 0o644)
		}
	}
	for _, tt := range []struct {
		name   string
		mutate func(*testing.T, *Options, *string)
		want   string
	}{
		{"darwin", func(_ *testing.T, o *Options, _ *string) { o.GOOS = "darwin" }, "only on Linux"},
		{"windows", func(_ *testing.T, o *Options, _ *string) { o.GOOS = "windows" }, "only on Linux"},
		{"empty home", func(_ *testing.T, o *Options, _ *string) { o.Home = "" }, "absolute"},
		{"relative home", func(_ *testing.T, o *Options, _ *string) { o.Home = "home" }, "absolute"},
		{"relative executable", func(_ *testing.T, o *Options, _ *string) { o.ExePath = "tailtab" }, "absolute"},
		{"absent executable", func(_ *testing.T, o *Options, _ *string) { o.ExePath += "-missing" }, "source executable"},
		{"directory executable", func(_ *testing.T, o *Options, _ *string) { o.ExePath = o.Home }, "regular executable"},
		{"bad id", func(_ *testing.T, o *Options, _ *string) { o.EdgeID = "invalid" }, "Chromium extension ID"},
		{"wrong id", func(_ *testing.T, o *Options, _ *string) { o.EdgeID = strings.Repeat("a", 32) }, "not --edge-id"},
		{"empty extension", func(_ *testing.T, _ *Options, dir *string) { *dir = "" }, "built Chromium"},
		{"absent extension", func(_ *testing.T, _ *Options, dir *string) { *dir += "-missing" }, "built Chromium"},
		{"file extension", func(_ *testing.T, o *Options, dir *string) { *dir = o.ExePath }, "built Chromium"},
		{"source ancestor", func(_ *testing.T, o *Options, dir *string) { *dir = o.Home }, "overlaps"},
		{"source filesystem root", func(_ *testing.T, o *Options, dir *string) {
			*dir = filepath.VolumeName(o.Home) + string(filepath.Separator)
		}, "overlaps"},
		{"source descendant", func(_ *testing.T, o *Options, dir *string) {
			*dir = filepath.Join(o.Home, ".var/app/com.google.Chrome/data/tailtab/extension/icons")
		}, "overlaps"},
		{"executable in extension", func(_ *testing.T, o *Options, _ *string) {
			o.ExePath = filepath.Join(o.Home, ".var/app/com.google.Chrome/data/tailtab/extension/background.js")
		}, "overlaps"},
		{"source manifests only", func(t *testing.T, _ *Options, dir *string) {
			if err := os.Rename(filepath.Join(*dir, "manifest.json"), filepath.Join(*dir, "manifest.chromium.json")); err != nil {
				t.Fatal(err)
			}
		}, "manifest.json"},
		{"malformed json", func(t *testing.T, _ *Options, dir *string) {
			flatpakWrite(t, filepath.Join(*dir, "manifest.json"), "{broken", 0o644)
		}, "manifest.json"},
		{"manifest v2", changeManifest("manifest_version", 2), "Manifest V3"},
		{"missing name", changeManifest("name", ""), "Manifest V3"},
		{"missing version", changeManifest("version", ""), "version"},
		{"no native messaging", changeManifest("permissions", []string{"storage"}), "native-messaging"},
		{"firefox settings", changeManifest("browser_specific_settings", map[string]any{"gecko": map[string]string{"id": "tailtab@stocist.dev"}}), "Firefox"},
		{"legacy gecko settings", changeManifest("applications", map[string]any{}), "Firefox"},
		{"firefox scripts", changeManifest("background", map[string]any{"scripts": []string{"background.js"}}), "Firefox"},
		{"mixed browser", changeManifest("background", map[string]any{"service_worker": "background.js", "scripts": []string{"background.js"}}), "Firefox"},
		{"missing key", changeManifest("key", ""), "DER public key"},
		{"bad base64", changeManifest("key", "not base64!"), "base64 DER public key"},
		{"not der", changeManifest("key", "YWJjZA=="), "DER public key"},
		{"missing worker", changeManifest("background", map[string]string{"service_worker": "absent.js"}), "regular file"},
		{"escaping worker", changeManifest("background", map[string]string{"service_worker": "../host"}), "inside the extension"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts, extension, app := flatpakFixture(t)
			if _, err := InstallChromeFlatpak(opts, extension); err != nil {
				t.Fatal(err)
			}
			nativeManifest := filepath.Join(app, "config/google-chrome/NativeMessagingHosts", manifestFile)
			oldManifest, err := os.ReadFile(nativeManifest)
			if err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(app, "data/tailtab/extension/old.js")
			flatpakWrite(t, stale, "keep old install", 0o644)
			flatpakWrite(t, opts.ExePath, "replacement host", 0o755)
			tt.mutate(t, &opts, &extension)
			paths, err := InstallChromeFlatpak(opts, extension)
			if err == nil || !strings.Contains(err.Error(), tt.want) || len(paths) != 0 {
				t.Fatalf("InstallChromeFlatpak = %v, %v; want error containing %q", paths, err, tt.want)
			}
			flatpakCheck(t, filepath.Join(app, "data/tailtab/bin/tailtab"), "host v1")
			flatpakCheck(t, filepath.Join(app, "data/tailtab/extension/background.js"), "original background.js")
			flatpakCheck(t, nativeManifest, string(oldManifest))
			flatpakCheck(t, stale, "keep old install")
			flatpakNoStage(t, app)
		})
	}
}

func TestInstallChromeFlatpakRejectsLinksAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"file link", "directory link", "dangling link", "executable link", "socket"} {
		t.Run(kind, func(t *testing.T) {
			opts, extension, app := flatpakFixture(t)
			if _, err := InstallChromeFlatpak(opts, extension); err != nil {
				t.Fatal(err)
			}
			if kind == "socket" {
				if runtime.GOOS == "windows" {
					t.Skip("Unix socket fixture")
				}
				t.Chdir(extension)
				listener, err := net.Listen("unix", "special.sock")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			} else {
				target := opts.ExePath
				if kind == "directory link" {
					target = opts.Home
				} else if kind == "dangling link" {
					target += "-absent"
				}
				link := filepath.Join(extension, "link")
				if err := os.Symlink(target, link); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlinks unavailable: %v", err)
					}
					t.Fatal(err)
				}
				if kind == "executable link" {
					opts.ExePath = link
				}
			}
			if paths, err := InstallChromeFlatpak(opts, extension); err == nil || len(paths) != 0 {
				t.Fatalf("accepted %s: %v, %v", kind, paths, err)
			}
			flatpakCheck(t, filepath.Join(app, "data/tailtab/bin/tailtab"), "host v1")
			flatpakCheck(t, filepath.Join(app, "data/tailtab/extension/background.js"), "original background.js")
			flatpakNoStage(t, app)
		})
	}
}

func TestInstallChromeFlatpakRejectsDestinationLinks(t *testing.T) {
	for _, path := range []string{"data", "data/tailtab", "data/tailtab/bin", "data/tailtab/bin/tailtab", "data/tailtab/extension", "config", "config/google-chrome", "config/google-chrome/NativeMessagingHosts", "config/google-chrome/NativeMessagingHosts/" + manifestFile} {
		t.Run(path, func(t *testing.T) {
			opts, extension, app := flatpakFixture(t)
			outside := filepath.Join(filepath.Dir(opts.Home), "other-app")
			flatpakWrite(t, filepath.Join(outside, "state"), "untouched", 0o600)
			target := outside
			if path == "data/tailtab/bin/tailtab" || strings.HasSuffix(path, ".json") {
				target = filepath.Join(outside, "state")
			}
			link := filepath.Join(app, path)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlinks unavailable: %v", err)
				}
				t.Fatal(err)
			}
			if paths, err := InstallChromeFlatpak(opts, extension); err == nil || !strings.Contains(err.Error(), "managed destination") || len(paths) != 0 {
				t.Fatalf("accepted destination symlink: %v, %v", paths, err)
			}
			flatpakCheck(t, filepath.Join(outside, "state"), "untouched")
			if _, err := os.Readlink(link); err != nil {
				t.Fatalf("destination link changed: %v", err)
			}
			flatpakNoStage(t, app)
		})
	}
}

func TestInstallChromeFlatpakSymlinkHome(t *testing.T) {
	opts, extension, app := flatpakFixture(t)
	alias := filepath.Join(filepath.Dir(opts.Home), "home-alias")
	if err := os.Symlink(opts.Home, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	opts.Home = alias
	if _, err := InstallChromeFlatpak(opts, extension); err != nil {
		t.Fatal(err)
	}
	m := decode(t, filepath.Join(app, "config/google-chrome/NativeMessagingHosts", manifestFile))
	if m.Path != filepath.Join(app, "data/tailtab/bin/tailtab") {
		t.Fatalf("manifest does not use canonical app root: %s", m.Path)
	}
}

func TestInstallChromeFlatpakRequiresApp(t *testing.T) {
	for _, regularFile := range []bool{false, true} {
		t.Run(fmt.Sprint(regularFile), func(t *testing.T) {
			opts, extension, app := flatpakFixture(t)
			if err := os.Remove(app); err != nil {
				t.Fatal(err)
			}
			if regularFile {
				flatpakWrite(t, app, "not an app directory", 0o600)
			}
			if paths, err := InstallChromeFlatpak(opts, extension); err == nil || len(paths) != 0 {
				t.Fatalf("missing app accepted: %v, %v", paths, err)
			}
			if regularFile {
				flatpakCheck(t, app, "not an app directory")
			} else if _, err := os.Stat(app); !os.IsNotExist(err) {
				t.Fatalf("installer created absent app directory: %v", err)
			}
		})
	}
}

func TestInstallChromeFlatpakRollback(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permissions enforced for a non-root user")
	}
	for _, blocked := range []string{"data/tailtab", "config/google-chrome/NativeMessagingHosts"} {
		t.Run(blocked, func(t *testing.T) {
			opts, extension, app := flatpakFixture(t)
			if _, err := InstallChromeFlatpak(opts, extension); err != nil {
				t.Fatal(err)
			}
			nativeManifest := filepath.Join(app, "config/google-chrome/NativeMessagingHosts", manifestFile)
			oldManifest, err := os.ReadFile(nativeManifest)
			if err != nil {
				t.Fatal(err)
			}
			flatpakWrite(t, opts.ExePath, "new host", 0o755)
			flatpakWrite(t, filepath.Join(extension, "background.js"), "new worker", 0o644)
			dir := filepath.Join(app, blocked)
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			paths, err := InstallChromeFlatpak(opts, extension)
			if err == nil || !strings.Contains(err.Error(), "replacing") || len(paths) != 0 {
				t.Fatalf("InstallChromeFlatpak = %v, %v; want replacement failure", paths, err)
			}
			flatpakCheck(t, filepath.Join(app, "data/tailtab/bin/tailtab"), "host v1")
			flatpakCheck(t, filepath.Join(app, "data/tailtab/extension/background.js"), "original background.js")
			flatpakCheck(t, nativeManifest, string(oldManifest))
			flatpakNoStage(t, app)
		})
	}
}
