package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidChromiumID(t *testing.T) {
	good := []string{
		"abcdefghijklmnopabcdefghijklmnop",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"pppppppppppppppppppppppppppppppp",
	}
	for _, id := range good {
		if !ValidChromiumID(id) {
			t.Errorf("ValidChromiumID(%q) = false, want true", id)
		}
	}
	bad := []string{
		"",
		"abcdefghijklmnopabcdefghijklmno",   // 31 characters
		"abcdefghijklmnopabcdefghijklmnopq", // 33 characters
		"abcdefghijklmnopabcdefghijklmnoq",  // q is outside a-p
		"ABCDEFGHIJKLMNOPABCDEFGHIJKLMNOP",  // uppercase
		"abcdefghijklmno0abcdefghijklmnop",  // digit
		"../../../../etc/passwd",
	}
	for _, id := range bad {
		if ValidChromiumID(id) {
			t.Errorf("ValidChromiumID(%q) = true, want false", id)
		}
	}
}

func TestValidGeckoID(t *testing.T) {
	good := []string{
		"tailtab@stocist.dev",
		"a+b@example.co.uk",
		"under_score@x-y.dev",
		"{0f8fad5b-d9cb-469f-a165-70867728950e}",
	}
	for _, id := range good {
		if !ValidGeckoID(id) {
			t.Errorf("ValidGeckoID(%q) = false, want true", id)
		}
	}
	bad := []string{
		"",
		"tailtab",                              // no domain
		"tailtab@",                             // no domain
		"@stocist.dev",                         // no local part
		"tailtab@stocist.dev/../x",             // path characters
		"tailtab @stocist.dev",                 // space
		"0f8fad5b-d9cb-469f-a165-70867728950e", // a UUID needs braces
		"{0F8FAD5B-D9CB-469F-A165-70867728950E}",
		"tailtab@stocist.dev\n",
	}
	for _, id := range bad {
		if ValidGeckoID(id) {
			t.Errorf("ValidGeckoID(%q) = true, want false", id)
		}
	}
}

func TestTargetsRejectsBadInput(t *testing.T) {
	base := Options{GOOS: "darwin", Home: "/home/u", ExePath: "/usr/local/bin/tailtab", EdgeID: "abcdefghijklmnopabcdefghijklmnop", GeckoID: "tailtab@stocist.dev"}
	if _, err := Targets(base); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Options){
		"no home":           func(o *Options) { o.Home = "" },
		"relative exe":      func(o *Options) { o.ExePath = "bin/tailtab" },
		"bad chromium id":   func(o *Options) { o.EdgeID = "not-an-id" },
		"bad gecko id":      func(o *Options) { o.GeckoID = "tailtab" },
		"empty chromium id": func(o *Options) { o.EdgeID = "" },
		"empty gecko id":    func(o *Options) { o.GeckoID = "" },
	} {
		opts := base
		mutate(&opts)
		if _, err := Targets(opts); err == nil {
			t.Errorf("%s: Targets accepted %+v", name, opts)
		}
	}
}

func TestInstallAndUninstall(t *testing.T) {
	home := t.TempDir()
	// Chrome is not installed here, but a Chrome-shaped directory holding a
	// sibling host proves the uninstaller leaves other manifests alone.
	otherHost := filepath.Join(home, "Library", "Application Support", "Microsoft Edge", "NativeMessagingHosts", "com.example.other.json")
	if err := os.MkdirAll(filepath.Dir(otherHost), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherHost, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		GOOS:    "darwin",
		Home:    home,
		ExePath: filepath.Join(home, "bin", "tailtab"),
		EdgeID:  "abcdefghijklmnopabcdefghijklmnop",
		GeckoID: "tailtab@stocist.dev",
	}
	written, err := Install(opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("Install wrote %v, want the Edge and Mozilla manifests only (Chrome is absent)", written)
	}

	edge := decode(t, filepath.Join(home, "Library", "Application Support", "Microsoft Edge", "NativeMessagingHosts", manifestFile))
	if got, want := edge.AllowedOrigins, []string{"chrome-extension://abcdefghijklmnopabcdefghijklmnop/"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Edge allowed_origins = %v, want %v", got, want)
	}
	if len(edge.AllowedExtensions) != 0 {
		t.Errorf("the Edge manifest carries allowed_extensions: %v", edge.AllowedExtensions)
	}
	if edge.Name != HostName || edge.Type != "stdio" || edge.Path != opts.ExePath {
		t.Errorf("Edge manifest = %+v", edge)
	}

	moz := decode(t, filepath.Join(home, "Library", "Application Support", "Mozilla", "NativeMessagingHosts", manifestFile))
	if got := moz.AllowedExtensions; len(got) != 1 || got[0] != "tailtab@stocist.dev" {
		t.Errorf("Mozilla allowed_extensions = %v", got)
	}
	if len(moz.AllowedOrigins) != 0 {
		t.Errorf("the Mozilla manifest carries allowed_origins: %v", moz.AllowedOrigins)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Application Support", "zen")); !os.IsNotExist(err) {
		t.Error("something was written under the zen application-support directory")
	}

	// A second install over the top must be idempotent.
	if _, err := Install(opts); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	removed, err := uninstall("darwin", home, "")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("Uninstall removed %v, want the two manifests it wrote", removed)
	}
	if _, err := os.Stat(otherHost); err != nil {
		t.Errorf("Uninstall removed a sibling manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Application Support", "Microsoft Edge", "NativeMessagingHosts")); err != nil {
		t.Errorf("Uninstall removed the directory itself: %v", err)
	}
	// Removing what is already gone is not an error.
	if removed, err := uninstall("darwin", home, ""); err != nil || len(removed) != 0 {
		t.Errorf("second Uninstall = %v, %v; want no files and no error", removed, err)
	}
}

func TestInstallRegistersChromeWhenPresent(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts"), 0o755); err != nil {
		t.Fatal(err)
	}
	written, err := Install(Options{
		GOOS:    "darwin",
		Home:    home,
		ExePath: "/opt/tailtab",
		EdgeID:  "abcdefghijklmnopabcdefghijklmnop",
		GeckoID: "{0f8fad5b-d9cb-469f-a165-70867728950e}",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(written) != 3 {
		t.Errorf("Install wrote %v, want three manifests with Chrome present", written)
	}
}

func decode(t *testing.T, path string) manifest {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return m
}

func TestTargetsPerPlatform(t *testing.T) {
	base := Options{Home: "/home/u", ExePath: "/opt/tailtab", EdgeID: "abcdefghijklmnopabcdefghijklmnop", GeckoID: "tailtab@stocist.dev"}

	linux := base
	linux.GOOS = "linux"
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := Targets(linux)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, tg := range got {
		paths[tg.Path()] = true
		if tg.Registry != "" {
			t.Errorf("linux target %s has a registry key", tg.Browser)
		}
	}
	for _, want := range []string{
		"/home/u/.config/microsoft-edge/NativeMessagingHosts/com.stocist.tailtab.json",
		"/home/u/.mozilla/native-messaging-hosts/com.stocist.tailtab.json",
		"/home/u/.config/google-chrome/NativeMessagingHosts/com.stocist.tailtab.json",
	} {
		if !paths[filepath.FromSlash(want)] {
			t.Errorf("linux targets lack %s; have %v", want, paths)
		}
	}

	win := base
	win.GOOS = "windows"
	win.Home = `C:\Users\u`
	win.LocalAppData = `C:\Users\u\AppData\Local`
	win.ExePath = `C:\Users\u\bin\tailtab.exe`
	got, err = Targets(win)
	if err != nil {
		t.Fatal(err)
	}
	var edge, gecko *Target
	for i := range got {
		if got[i].Registry == "" {
			t.Errorf("windows target %s has no registry key", got[i].Browser)
		}
		if got[i].Browser == "Microsoft Edge" {
			edge = &got[i]
		}
		if got[i].Browser == "Zen and Firefox" {
			gecko = &got[i]
		}
	}
	if edge == nil || gecko == nil {
		t.Fatal("windows targets lack Edge or Gecko")
	}
	if edge.Registry != `Software\Microsoft\Edge\NativeMessagingHosts\com.stocist.tailtab` {
		t.Errorf("edge registry key = %q", edge.Registry)
	}
	if gecko.Registry != `Software\Mozilla\NativeMessagingHosts\com.stocist.tailtab` {
		t.Errorf("gecko registry key = %q", gecko.Registry)
	}
	if edge.File == gecko.File {
		t.Error("the Chromium and Gecko manifests share a file name on Windows; they differ in content")
	}
	if len(edge.manifest.AllowedOrigins) != 1 || len(gecko.manifest.AllowedExtensions) != 1 {
		t.Error("manifests were not assigned per browser family")
	}

	other := base
	other.GOOS = "plan9"
	if _, err := Targets(other); err == nil {
		t.Error("an unsupported platform was accepted")
	}
}

func TestLinuxInstallWritesOnlySupportedBrowsersByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	written, err := Install(Options{GOOS: "linux", Home: home, ExePath: "/opt/tailtab", EdgeID: "abcdefghijklmnopabcdefghijklmnop", GeckoID: "tailtab@stocist.dev"})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Fatalf("wrote %v, want the Edge and Mozilla manifests only", written)
	}
	removed, err := uninstall("linux", home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v, want both", removed)
	}
}
