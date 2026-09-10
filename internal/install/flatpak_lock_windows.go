package install

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockFlatpakFile(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err == windows.ERROR_LOCK_VIOLATION {
		return fmt.Errorf("%w: %w", errFlatpakInstallBusy, err)
	}
	return err
}

func unlockFlatpakFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
