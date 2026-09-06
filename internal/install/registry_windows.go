//go:build windows

package install

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

func setRegistryPath(key, path string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("", path)
}

func deleteRegistryKey(key string) error {
	err := registry.DeleteKey(registry.CURRENT_USER, key)
	if err == nil || errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

func registryKeyExists(key string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}
