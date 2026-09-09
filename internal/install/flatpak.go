package install

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// InstallChromeFlatpak requires Chrome to be closed during upgrades because the
// host, extension, and manifest cannot all be replaced in one atomic operation.
func InstallChromeFlatpak(opts Options, extensionDir string) ([]string, error) {
	if opts.goos() != "linux" {
		return nil, errors.New("Chrome Flatpak installation is supported only on Linux")
	}
	if !filepath.IsAbs(opts.Home) || !filepath.IsAbs(opts.ExePath) {
		return nil, errors.New("Chrome Flatpak installation requires absolute home and executable paths")
	}
	if !ValidChromiumID(opts.EdgeID) {
		return nil, fmt.Errorf("--edge-id %q is not a Chromium extension ID (32 characters, a-p)", opts.EdgeID)
	}
	if extensionDir == "" {
		return nil, errors.New("a built Chromium extension directory is required")
	}
	appPath, err := filepath.EvalSymlinks(filepath.Join(opts.Home, ".var", "app", "com.google.Chrome"))
	if err != nil {
		return nil, fmt.Errorf("install and launch com.google.Chrome before installing its native host: %w", err)
	}
	app, err := os.OpenRoot(appPath)
	if err != nil {
		return nil, fmt.Errorf("opening Chrome Flatpak app directory: %w", err)
	}
	defer app.Close()
	release, err := lockFlatpakApp(app)
	if err != nil {
		return nil, fmt.Errorf("locking Chrome Flatpak installation: %w", err)
	}
	// Registered before staging so rollback and cleanup finish while still locked.
	defer release()

	paths := []string{
		"data/tailtab/bin/tailtab",
		"data/tailtab/extension",
		"config/google-chrome/NativeMessagingHosts/" + manifestFile,
	}
	for i, path := range paths {
		parts := strings.Split(path, "/")
		for j := range parts {
			p := strings.Join(parts[:j+1], "/")
			info, err := app.Lstat(p)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("checking managed destination %s: %w", p, err)
			}
			wantDir := j < len(parts)-1 || i == 1
			if info.Mode()&os.ModeSymlink != 0 || (wantDir && !info.IsDir()) || (!wantDir && !info.Mode().IsRegular()) {
				return nil, fmt.Errorf("managed destination %s has an unexpected type; symlinks and special files are not supported", p)
			}
		}
	}

	extensionDir, err = filepath.Abs(extensionDir)
	if err != nil {
		return nil, err
	}
	extensionDir, err = filepath.EvalSymlinks(extensionDir)
	if err != nil {
		return nil, fmt.Errorf("opening built Chromium extension: %w", err)
	}
	exeInfo, err := os.Lstat(opts.ExePath)
	if err != nil || !exeInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("source executable %q must be a regular executable file", opts.ExePath)
	}
	exePath, err := filepath.EvalSymlinks(opts.ExePath)
	if err != nil {
		return nil, fmt.Errorf("opening source executable: %w", err)
	}
	overlaps := func(a, b string) bool {
		ab, errAB := filepath.Rel(a, b)
		ba, errBA := filepath.Rel(b, a)
		return (errAB == nil && filepath.IsLocal(ab)) || (errBA == nil && filepath.IsLocal(ba))
	}
	for i, path := range paths {
		dest := filepath.Join(appPath, path)
		if (overlaps(extensionDir, dest) && !(i == 1 && extensionDir == dest)) ||
			(overlaps(exePath, dest) && !(i == 0 && exePath == dest)) {
			return nil, fmt.Errorf("source overlaps managed destination %s; use a separate build directory or the exact installed extension and host paths", dest)
		}
	}
	exe, err := os.Open(exePath)
	if err != nil {
		return nil, fmt.Errorf("opening source executable: %w", err)
	}
	defer exe.Close()
	source, err := os.OpenRoot(extensionDir)
	if err != nil {
		return nil, fmt.Errorf("opening built Chromium extension: %w", err)
	}
	defer source.Close()

	stage := ".tailtab-install-" + rand.Text()
	if err := app.Mkdir(stage, 0o700); err != nil {
		return nil, fmt.Errorf("staging Chrome Flatpak installation: %w", err)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = app.RemoveAll(stage)
		}
	}()
	copyFile := func(in *os.File, path string, mode fs.FileMode) error {
		info, err := in.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", in.Name())
		}
		out, err := app.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Close())
	}
	if err := copyFile(exe, stage+"/host", 0o755); err != nil {
		return nil, fmt.Errorf("staging native host: %w", err)
	}
	if err := exe.Close(); err != nil {
		return nil, fmt.Errorf("closing source executable: %w", err)
	}
	err = fs.WalkDir(source.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(stage, "extension", filepath.FromSlash(path))
		if entry.IsDir() {
			return app.Mkdir(dest, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("extension entry %s must be a regular file or directory; symlinks and special files are not supported", path)
		}
		in, err := source.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		return copyFile(in, dest, 0o644)
	})
	if err != nil {
		return nil, fmt.Errorf("staging built Chromium extension: %w", err)
	}
	if err := source.Close(); err != nil {
		return nil, fmt.Errorf("closing source extension: %w", err)
	}
	if err := validateFlatpakExtension(app, stage+"/extension", opts.EdgeID); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(manifest{
		Name:           HostName,
		Description:    description,
		Path:           filepath.Join(appPath, paths[0]),
		Type:           "stdio",
		AllowedOrigins: []string{"chrome-extension://" + opts.EdgeID + "/"},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := app.WriteFile(stage+"/manifest", append(b, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("staging native messaging manifest: %w", err)
	}

	var created []string
	committed := false
	defer func() {
		if !committed {
			for _, dir := range slices.Backward(created) {
				_ = app.Remove(dir)
			}
		}
	}()
	for _, dir := range []string{"data", "data/tailtab", "data/tailtab/bin", "config", "config/google-chrome", "config/google-chrome/NativeMessagingHosts"} {
		if err := app.Mkdir(dir, 0o755); err == nil {
			created = append(created, dir)
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	staged := []string{stage + "/host", stage + "/extension", stage + "/manifest"}
	saved, replaced := [3]bool{}, [3]bool{}
	for i, path := range paths {
		if _, err = app.Lstat(path); err == nil {
			err = app.Rename(path, stage+"/old-"+strconv.Itoa(i))
			saved[i] = err == nil
		} else if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err == nil {
			err = app.Rename(staged[i], path)
			replaced[i] = err == nil
		}
		if err != nil {
			err = fmt.Errorf("replacing %s (close Chrome before upgrading): %w", path, err)
			break
		}
	}
	if err != nil {
		var rollbackErr error
		for i := len(paths) - 1; i >= 0; i-- {
			if replaced[i] {
				rollbackErr = errors.Join(rollbackErr, app.RemoveAll(paths[i]))
			}
			if saved[i] {
				rollbackErr = errors.Join(rollbackErr, app.Rename(stage+"/old-"+strconv.Itoa(i), paths[i]))
			}
		}
		if rollbackErr != nil {
			keepStage = true
			err = errors.Join(err, fmt.Errorf("rollback incomplete; recovery files retained in %s: %w", filepath.Join(appPath, stage), rollbackErr))
		}
		return nil, err
	}
	committed = true
	for i := range paths {
		paths[i] = filepath.Join(appPath, paths[i])
	}
	return paths, nil
}

func validateFlatpakExtension(app *os.Root, dir, id string) error {
	b, err := app.ReadFile(dir + "/manifest.json")
	if err != nil {
		return fmt.Errorf("a built Chromium extension with manifest.json is required: %w", err)
	}
	var m struct {
		ManifestVersion         int             `json:"manifest_version"`
		Name                    string          `json:"name"`
		Version                 string          `json:"version"`
		Key                     string          `json:"key"`
		BrowserSpecificSettings json.RawMessage `json:"browser_specific_settings"`
		Applications            json.RawMessage `json:"applications"`
		Permissions             []string        `json:"permissions"`
		Background              struct {
			ServiceWorker string          `json:"service_worker"`
			Scripts       json.RawMessage `json:"scripts"`
		} `json:"background"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("invalid Chromium manifest.json: %w", err)
	}
	if m.ManifestVersion != 3 || strings.TrimSpace(m.Name) == "" || m.Background.ServiceWorker == "" ||
		len(m.BrowserSpecificSettings) != 0 || len(m.Applications) != 0 || len(m.Background.Scripts) != 0 ||
		!slices.Contains(m.Permissions, "nativeMessaging") {
		return errors.New("manifest.json must describe a Chromium Manifest V3 native-messaging extension with a service worker, not a Firefox bundle")
	}
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("manifest.json must include a nonempty version")
	}
	key, err := base64.StdEncoding.DecodeString(m.Key)
	if err != nil {
		return fmt.Errorf("manifest.json key must be a base64 DER public key: %w", err)
	}
	if _, err := x509.ParsePKIXPublicKey(key); err != nil {
		return fmt.Errorf("manifest.json key must be a DER public key: %w", err)
	}
	hash := sha256.Sum256(key)
	var derived [32]byte
	for i, b := range hash[:16] {
		derived[2*i], derived[2*i+1] = 'a'+(b>>4), 'a'+(b&15)
	}
	if string(derived[:]) != id {
		return fmt.Errorf("manifest.json key gives extension ID %s, not --edge-id %s", derived, id)
	}
	worker := m.Background.ServiceWorker
	if !fs.ValidPath(worker) || strings.Contains(worker, `\`) {
		return fmt.Errorf("manifest.json service worker %q must stay inside the extension", worker)
	}
	info, err := app.Lstat(dir + "/" + worker)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("manifest.json service worker %q must exist as a regular file in the built extension", worker)
	}
	return nil
}
