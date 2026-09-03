//go:build !windows

package install

import "errors"

// Only Windows targets carry a registry key, so these are never reached off
// Windows; they exist so the package compiles everywhere.
func setRegistryPath(key, path string) error {
	return errors.New("registry keys exist only on Windows")
}

func deleteRegistryKey(key string) error { return nil }
