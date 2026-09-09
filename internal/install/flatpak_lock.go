package install

import (
	"errors"
	"fmt"
	"os"
)

const flatpakLockName = ".tailtab-install.lock"

var errFlatpakInstallBusy = errors.New("another Chrome Flatpak installation is in progress; wait for it to finish and retry")

func lockFlatpakApp(app *os.Root) (release func(), err error) {
	info, err := app.Lstat(flatpakLockName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checking %s: %w", flatpakLockName, err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file, not a symlink or special file", flatpakLockName)
	}
	// Never truncate, replace, or unlink: cooperating installers must share one inode.
	file, err := app.OpenFile(flatpakLockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", flatpakLockName, err)
	}
	defer func() {
		if err != nil {
			_ = file.Close()
		}
	}()
	info, err = file.Stat()
	if err != nil {
		return nil, fmt.Errorf("checking open %s: %w", flatpakLockName, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open %s must be a regular file", flatpakLockName)
	}
	entry, err := app.Lstat(flatpakLockName)
	if err != nil {
		return nil, fmt.Errorf("rechecking %s: %w", flatpakLockName, err)
	}
	if !entry.Mode().IsRegular() || !os.SameFile(info, entry) {
		return nil, fmt.Errorf("%s changed while opening", flatpakLockName)
	}
	if err := lockFlatpakFile(file); err != nil {
		return nil, fmt.Errorf("locking %s: %w", flatpakLockName, err)
	}
	return func() {
		_ = unlockFlatpakFile(file)
		_ = file.Close()
	}, nil
}
