//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package install

import (
	"errors"
	"os"
)

func lockFlatpakFile(*os.File) error {
	return errors.New("Chrome Flatpak installation locking is unsupported on this platform")
}

func unlockFlatpakFile(*os.File) error { return nil }
