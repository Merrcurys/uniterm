//go:build !android

package sync

// Platform integration for the sync package: where sync metadata lives, and
// which secure storage backs the keychain. Everything else in the package is
// platform-neutral.

import (
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// syncMetaDir resolves the directory holding sync metadata (sync-config.json
// and the local snapshot repo clone) via the standard user config dir.
func syncMetaDir() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cfgDir, "uniTerm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func keychainGet(service, key string) (string, error) {
	return keyring.Get(service, key)
}

func keychainSet(service, key, value string) error {
	return keyring.Set(service, key, value)
}

func keychainDelete(service, key string) error {
	return keyring.Delete(service, key)
}
