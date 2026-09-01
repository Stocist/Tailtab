// Package install writes and removes the native-messaging host manifests that
// let a browser start tailtab.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// HostName is the native-messaging host name. The extension passes this exact
// string to connectNative, and it is also the manifest's file name.
const HostName = "com.stocist.tailtab"

// manifestFile is the file written into each browser's directory. Uninstall
// removes this name and nothing else: these directories are shared with other
// applications' hosts.
const manifestFile = HostName + ".json"

const description = "tailtab: a Tailscale node for one browser profile"

// chromiumIDRE matches an unpacked or store Chromium extension ID: 32
// characters from a-p, the base-16 alphabet Chromium shifts into a-p.
var chromiumIDRE = regexp.MustCompile(`^[a-p]{32}$`)

// geckoIDRE and geckoUUIDRE match the two legal shapes of an add-on ID: an
// email-like string, or a brace-wrapped UUID.
var (
	geckoIDRE   = regexp.MustCompile(`^[\w.+-]+@[\w.-]+$`)
	geckoUUIDRE = regexp.MustCompile(`^\{[0-9a-f-]{36}\}$`)
)

// ValidChromiumID reports whether id is a well-formed Chromium extension ID.
func ValidChromiumID(id string) bool { return chromiumIDRE.MatchString(id) }

// ValidGeckoID reports whether id is a well-formed Firefox add-on ID.
func ValidGeckoID(id string) bool {
	return geckoIDRE.MatchString(id) || geckoUUIDRE.MatchString(id)
}

// manifest is the on-disk native-messaging manifest. The two allowed-caller
// fields are mutually exclusive: Chromium reads AllowedOrigins, Gecko reads
// AllowedExtensions.
type manifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	// AllowedOrigins entries must carry a trailing slash or Chromium rejects them.
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// Target is one browser's manifest directory and the manifest to write there.
type Target struct {
	// Browser is a human-readable name for logging.
	Browser string
	// Dir is the directory the manifest belongs in.
	Dir string
	// Create says whether to create Dir if it is missing. It is false for
	// browsers we support opportunistically but do not require.
	Create bool

	manifest manifest
}

// Path returns the full path of the manifest file.
func (t Target) Path() string { return filepath.Join(t.Dir, manifestFile) }

// Options describes an install.
type Options struct {
	// Home is the user's home directory. Tests override it.
	Home string
	// ExePath is the absolute path of the tailtab binary the browser will run.
	ExePath string
	// EdgeID is the Chromium extension ID; GeckoID is the add-on ID.
	EdgeID  string
	GeckoID string
}

// dirs, relative to the home directory. Zen reads only Mozilla's directory: the
// "Mozilla" component is a hardcoded literal in Gecko's directory provider, so
// setting Profile=zen moves the profile but not the manifests
// (research/browser.md §3.3, zen-browser/desktop#13214). Nothing is ever
// written under ~/Library/Application Support/zen/.
var (
	edgeDir    = filepath.Join("Library", "Application Support", "Microsoft Edge", "NativeMessagingHosts")
	mozillaDir = filepath.Join("Library", "Application Support", "Mozilla", "NativeMessagingHosts")
	chromeDir  = filepath.Join("Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")
)

// Targets returns the manifests to write for opts, in the order they are
// written. It validates the IDs and touches no files, so it is also the
// dry-run view of an install.
func Targets(opts Options) ([]Target, error) {
	if opts.Home == "" {
		return nil, errors.New("no home directory")
	}
	if !filepath.IsAbs(opts.ExePath) {
		return nil, fmt.Errorf("the binary path %q is not absolute", opts.ExePath)
	}
	if !ValidChromiumID(opts.EdgeID) {
		return nil, fmt.Errorf("--edge-id %q is not a Chromium extension ID (32 characters, a-p)", opts.EdgeID)
	}
	if !ValidGeckoID(opts.GeckoID) {
		return nil, fmt.Errorf("--gecko-id %q is not an add-on ID (name@domain or {uuid})", opts.GeckoID)
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

	targets := []Target{
		{Browser: "Microsoft Edge", Dir: filepath.Join(opts.Home, edgeDir), Create: true, manifest: chromium},
		{Browser: "Zen and Firefox", Dir: filepath.Join(opts.Home, mozillaDir), Create: true, manifest: gecko},
		// Chrome is a bonus: register there if the user has Chrome, but never
		// create the directory for a browser that is not installed.
		{Browser: "Google Chrome", Dir: filepath.Join(opts.Home, chromeDir), Create: false, manifest: chromium},
	}
	return targets, nil
}

// Install writes the manifests and returns the paths written.
func Install(opts Options) ([]string, error) {
	targets, err := Targets(opts)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, t := range targets {
		if _, err := os.Stat(t.Dir); err != nil {
			if !t.Create {
				continue // that browser is not installed
			}
			if err := os.MkdirAll(t.Dir, 0o755); err != nil {
				return written, fmt.Errorf("creating %s: %w", t.Dir, err)
			}
		}
		b, err := json.MarshalIndent(t.manifest, "", "  ")
		if err != nil {
			return written, err
		}
		b = append(b, '\n')
		if err := os.WriteFile(t.Path(), b, 0o644); err != nil {
			return written, fmt.Errorf("writing %s: %w", t.Path(), err)
		}
		written = append(written, t.Path())
	}
	return written, nil
}

// Uninstall removes our manifest from every directory it could have been
// written to, leaving other hosts' manifests alone. A missing file is not an
// error.
func Uninstall(home string) ([]string, error) {
	if home == "" {
		return nil, errors.New("no home directory")
	}
	var removed []string
	for _, d := range []string{edgeDir, mozillaDir, chromeDir} {
		p := filepath.Join(home, d, manifestFile)
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

// ExePath returns the absolute path of the running binary, with symlinks
// resolved, for the manifest's "path" field.
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
