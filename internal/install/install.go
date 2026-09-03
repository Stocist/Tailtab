// Package install writes and removes the native-messaging manifests that let a
// browser start the tailtab host.
//
// Every browser looks for a manifest named after the host in a directory (or,
// on Windows, a registry key) of its own. The manifest names the absolute path
// of the binary and the one extension allowed to talk to it.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

// HostName is the native-messaging host name the extension connects to.
const HostName = "com.stocist.tailtab"

const manifestFile = HostName + ".json"
const description = "tailtab: a Tailscale node for one browser profile"

// A Chromium extension ID is 32 characters of a-p (a hex digest mapped onto
// letters).
var chromiumIDRE = regexp.MustCompile(`^[a-p]{32}$`)

// A Gecko add-on ID is either name@domain or a braced UUID.
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

// manifest is the JSON both browser families read. Chromium wants
// allowed_origins, Gecko wants allowed_extensions; each target uses one.
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
	// Browser is a label for messages, e.g. "Microsoft Edge".
	Browser string
	// Dir is where the manifest file lives.
	Dir string
	// File is the manifest's file name inside Dir.
	File string
	// Create is whether Dir may be created when it does not exist. It is
	// true for the browsers tailtab supports and false for bonus browsers, so
	// a directory is never created for a browser that is not installed.
	Create bool
	// Registry is the HKCU key that must point at the manifest, on Windows
	// only; empty elsewhere. Windows browsers find hosts through the
	// registry, not a directory.
	Registry string

	manifest manifest
}

// Path is the manifest's full path.
func (t Target) Path() string { return filepath.Join(t.Dir, t.File) }

// Options describes an installation.
type Options struct {
	// Home is the user's home directory.
	Home string
	// ExePath is the absolute path of the tailtab binary.
	ExePath string
	// EdgeID is the Chromium extension ID; GeckoID the Firefox/Zen add-on ID.
	EdgeID  string
	GeckoID string
	// GOOS overrides the platform, for tests. Empty means runtime.GOOS.
	GOOS string
	// LocalAppData is Windows's %LOCALAPPDATA%; empty means the environment
	// or the conventional path under Home.
	LocalAppData string
}

func (o Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}

// dirs lists every directory or registry location a platform's browsers read,
// without the manifests, so uninstall can use the same list.
func dirs(goos, home, localAppData string) []Target {
	switch goos {
	case "darwin":
		as := filepath.Join(home, "Library", "Application Support")
		return []Target{
			{Browser: "Microsoft Edge", Dir: filepath.Join(as, "Microsoft Edge", "NativeMessagingHosts"), File: manifestFile, Create: true},
			// Zen reads Mozilla's directory and only that one, so this covers
			// Zen and Firefox both. The directory does not exist until some
			// host creates it; never write under Application Support/zen/.
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
			// Firefox, Zen and every other Gecko browser read ~/.mozilla.
			{Browser: "Zen and Firefox", Dir: filepath.Join(home, ".mozilla", "native-messaging-hosts"), File: manifestFile, Create: true},
			{Browser: "Google Chrome", Dir: filepath.Join(cfg, "google-chrome", "NativeMessagingHosts"), File: manifestFile},
			{Browser: "Chromium", Dir: filepath.Join(cfg, "chromium", "NativeMessagingHosts"), File: manifestFile},
			{Browser: "Brave", Dir: filepath.Join(cfg, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts"), File: manifestFile},
		}
	case "windows":
		// Windows browsers find a host through HKCU\Software\<vendor>\
		// NativeMessagingHosts\<host name>, whose default value is the path
		// of the manifest. The files themselves live in one directory of
		// ours. The two families need different manifests, hence two files.
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
			{Browser: "Google Chrome", Dir: dir, File: chromium, Create: true, Registry: `Software\Google\Chrome\NativeMessagingHosts\` + HostName},
			{Browser: "Chromium", Dir: dir, File: chromium, Create: true, Registry: `Software\Chromium\NativeMessagingHosts\` + HostName},
			{Browser: "Brave", Dir: dir, File: chromium, Create: true, Registry: `Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + HostName},
		}
	}
	return nil
}

// windowsAbsRE matches a drive-letter or UNC path.
var windowsAbsRE = regexp.MustCompile(`^(?:[A-Za-z]:\\|\\\\)`)

// isAbs is filepath.IsAbs for the target platform rather than the running
// one, so a Windows layout can be validated (and tested) from anywhere.
func isAbs(goos, p string) bool {
	if goos == "windows" {
		return windowsAbsRE.MatchString(p)
	}
	return filepath.IsAbs(p)
}

// Targets validates opts and returns every manifest that would be written.
// Nothing touches the filesystem.
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
		if _, err := os.Stat(t.Dir); err != nil {
			if !t.Create {
				continue // that browser is not installed
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
			// nothing to do
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
