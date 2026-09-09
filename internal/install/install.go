// Package install manages browser-native messaging manifests. Each manifest
// pins the host binary and the extension allowed to launch it.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// HostName is the native-messaging host name the extension connects to.
const HostName = "com.stocist.tailtab"

const manifestFile = HostName + ".json"
const description = "tailtab: a Tailscale node for one browser profile"

var chromiumIDRE = regexp.MustCompile(`^[a-p]{32}$`)

var (
	geckoIDRE   = regexp.MustCompile(`^[\w.+-]+@[\w.-]+$`)
	geckoUUIDRE = regexp.MustCompile(`^\{[0-9a-f-]{36}\}$`)
)

// ValidChromiumID reports whether id is a Chromium extension ID.
func ValidChromiumID(id string) bool { return chromiumIDRE.MatchString(id) }

// ValidGeckoID reports whether id is a Firefox/Zen add-on ID.
func ValidGeckoID(id string) bool {
	return geckoIDRE.MatchString(id) || geckoUUIDRE.MatchString(id)
}

type manifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Type        string `json:"type"`

	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// Target is one place a manifest goes.
type Target struct {
	Browser string
	Dir     string
	File    string
	// Create permits creating Dir for explicitly supported browsers.
	Create bool
	// Registry is the Windows HKCU key pointing to the manifest.
	Registry string
	// Probe prevents registering optional Windows browsers that are absent.
	Probe string

	manifest manifest
}

// Path is the manifest's full path.
func (t Target) Path() string { return filepath.Join(t.Dir, t.File) }

// Options describes an installation.
type Options struct {
	Home    string
	ExePath string
	EdgeID  string
	GeckoID string
	// GOOS overrides runtime.GOOS for tests.
	GOOS string
	// LocalAppData overrides the Windows manifest directory.
	LocalAppData string
}

func (o Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}

func dirs(goos, home, localAppData string) []Target {
	switch goos {
	case "darwin":
		as := filepath.Join(home, "Library", "Application Support")
		return []Target{
			{Browser: "Microsoft Edge", Dir: filepath.Join(as, "Microsoft Edge", "NativeMessagingHosts"), File: manifestFile, Create: true},
			// Zen and Firefox both read Mozilla's directory on macOS.
			{Browser: "Zen and Firefox", Dir: filepath.Join(as, "Mozilla", "NativeMessagingHosts"), File: manifestFile, Create: true},
			{Browser: "Google Chrome", Dir: filepath.Join(as, "Google", "Chrome", "NativeMessagingHosts"), File: manifestFile},
			{Browser: "Chromium", Dir: filepath.Join(as, "Chromium", "NativeMessagingHosts"), File: manifestFile},
			{Browser: "Brave", Dir: filepath.Join(as, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts"), File: manifestFile},
		}
	case "linux":
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" || !filepath.IsAbs(cfg) {
			cfg = filepath.Join(home, ".config")
		}
		return []Target{
			{Browser: "Microsoft Edge", Dir: filepath.Join(cfg, "microsoft-edge", "NativeMessagingHosts"), File: manifestFile, Create: true},
			// Gecko browsers share ~/.mozilla on Linux.
			{Browser: "Zen and Firefox", Dir: filepath.Join(home, ".mozilla", "native-messaging-hosts"), File: manifestFile, Create: true},
			{Browser: "Google Chrome", Dir: filepath.Join(cfg, "google-chrome", "NativeMessagingHosts"), File: manifestFile},
			{Browser: "Chromium", Dir: filepath.Join(cfg, "chromium", "NativeMessagingHosts"), File: manifestFile},
			{Browser: "Brave", Dir: filepath.Join(cfg, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts"), File: manifestFile},
		}
	case "windows":
		// Windows discovers hosts through HKCU; Chromium and Gecko need separate
		// manifest files because their allowlist fields differ.
		if localAppData == "" {
			localAppData = os.Getenv("LOCALAPPDATA")
		}
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		dir := filepath.Join(localAppData, "tailtab")
		chromium := HostName + ".chromium.json"
		gecko := HostName + ".gecko.json"
		return []Target{
			{Browser: "Microsoft Edge", Dir: dir, File: chromium, Create: true, Registry: `Software\Microsoft\Edge\NativeMessagingHosts\` + HostName},
			{Browser: "Zen and Firefox", Dir: dir, File: gecko, Create: true, Registry: `Software\Mozilla\NativeMessagingHosts\` + HostName},
			// Optional browsers are registered only when their own key exists.
			{Browser: "Google Chrome", Dir: dir, File: chromium, Registry: `Software\Google\Chrome\NativeMessagingHosts\` + HostName, Probe: `Software\Google\Chrome`},
			{Browser: "Chromium", Dir: dir, File: chromium, Registry: `Software\Chromium\NativeMessagingHosts\` + HostName, Probe: `Software\Chromium`},
			{Browser: "Brave", Dir: dir, File: chromium, Registry: `Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + HostName, Probe: `Software\BraveSoftware\Brave-Browser`},
		}
	}
	return nil
}

var windowsAbsRE = regexp.MustCompile(`^(?:[A-Za-z]:[\\/]|\\\\)`)

// isAbs validates paths for the target platform, enabling cross-platform tests.
func isAbs(goos, p string) bool {
	if goos == "windows" {
		return windowsAbsRE.MatchString(p)
	}
	// Not filepath.IsAbs: that answers for the build host, and the tests run
	// the darwin and linux layouts on Windows too.
	return strings.HasPrefix(p, "/")
}

// Targets validates opts and returns targets without touching the filesystem.
func Targets(opts Options) ([]Target, error) {
	if opts.Home == "" {
		return nil, errors.New("no home directory")
	}
	if !isAbs(opts.goos(), opts.ExePath) {
		return nil, fmt.Errorf("the binary path %q is not absolute", opts.ExePath)
	}
	if !ValidChromiumID(opts.EdgeID) {
		return nil, fmt.Errorf("--edge-id %q is not a Chromium extension ID (32 characters, a-p)", opts.EdgeID)
	}
	if !ValidGeckoID(opts.GeckoID) {
		return nil, fmt.Errorf("--gecko-id %q is not an add-on ID (name@domain or {uuid})", opts.GeckoID)
	}
	goos := opts.goos()
	targets := dirs(goos, opts.Home, opts.LocalAppData)
	if targets == nil {
		return nil, fmt.Errorf("tailtab has no installer for %s", goos)
	}

	chromium := manifest{
		Name:           HostName,
		Description:    description,
		Path:           opts.ExePath,
		Type:           "stdio",
		AllowedOrigins: []string{"chrome-extension://" + opts.EdgeID + "/"},
	}
	gecko := manifest{
		Name:              HostName,
		Description:       description,
		Path:              opts.ExePath,
		Type:              "stdio",
		AllowedExtensions: []string{opts.GeckoID},
	}
	for i := range targets {
		if targets[i].Browser == "Zen and Firefox" {
			targets[i].manifest = gecko
		} else {
			targets[i].manifest = chromium
		}
	}
	return targets, nil
}

// Install writes the manifests (and, on Windows, the registry keys). It returns
// every path written, including those written before an error.
func Install(opts Options) ([]string, error) {
	targets, err := Targets(opts)
	if err != nil {
		return nil, err
	}
	var written []string
	wroteFile := map[string]bool{}
	for _, t := range targets {
		if t.Probe != "" && !registryKeyExists(t.Probe) {
			continue
		}
		if _, err := os.Stat(t.Dir); err != nil {
			if !t.Create && t.Registry == "" {
				continue
			}
			if err := os.MkdirAll(t.Dir, 0o755); err != nil {
				return written, fmt.Errorf("creating %s: %w", t.Dir, err)
			}
		}
		p := t.Path()
		if !wroteFile[p] {
			b, err := json.MarshalIndent(t.manifest, "", "  ")
			if err != nil {
				return written, err
			}
			b = append(b, '\n')
			if err := os.WriteFile(p, b, 0o644); err != nil {
				return written, fmt.Errorf("writing %s: %w", p, err)
			}
			wroteFile[p] = true
			written = append(written, p)
		}
		if t.Registry != "" {
			if err := setRegistryPath(t.Registry, p); err != nil {
				return written, fmt.Errorf("registering %s: %w", t.Browser, err)
			}
			written = append(written, `HKCU\`+t.Registry)
		}
	}
	return written, nil
}

// Uninstall removes exactly the files (and registry keys) Install writes,
// leaving every other host's manifest in place. Missing ones are not errors.
func Uninstall(home string) ([]string, error) {
	return uninstall(runtime.GOOS, home, "")
}

func uninstall(goos, home, localAppData string) ([]string, error) {
	if home == "" {
		return nil, errors.New("no home directory")
	}
	var removed []string
	seen := map[string]bool{}
	for _, t := range dirs(goos, home, localAppData) {
		if t.Registry != "" {
			if err := deleteRegistryKey(t.Registry); err != nil {
				return removed, fmt.Errorf("unregistering %s: %w", t.Browser, err)
			}
		}
		p := t.Path()
		if seen[p] {
			continue
		}
		seen[p] = true
		err := os.Remove(p)
		switch {
		case err == nil:
			removed = append(removed, p)
		case errors.Is(err, os.ErrNotExist):
		default:
			return removed, fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return removed, nil
}

// ExePath returns the absolute, symlink-resolved path of the running binary.
func ExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}
