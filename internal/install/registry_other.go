//go:build !windows

package install

import "errors"

// Registry helpers are unreachable for non-Windows targets.
func setRegistryPath(key, path string) error {
	return errors.New("registry keys exist only on Windows")
}

func deleteRegistryKey(key string) error { return nil }

func registryKeyExists(key string) bool { return false }
