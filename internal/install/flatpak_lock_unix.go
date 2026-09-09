//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package install

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockFlatpakFile(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == unix.EINTR {
			continue
		}
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return fmt.Errorf("%w: %w", errFlatpakInstallBusy, err)
		}
		return err
	}
}

func unlockFlatpakFile(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		if err != unix.EINTR {
			return err
		}
	}
}
