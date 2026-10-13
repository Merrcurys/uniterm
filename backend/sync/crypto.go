package sync

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// PasswordStore is the local credential store as sync sees it. Sync only
// consults Unlocked: every field-level concern (which fields are secret,
// enc:v1: encryption, legacy keychain backfill) lives in the stores, so a
// locked vault is the only thing that can still make sync misbehave.
type PasswordStore interface {
	// Unlocked reports whether the credential store currently holds a usable
	// key. When false (master-password not yet entered, keychain lost, or never
	// set up), sync must refuse to run rather than leak or mis-carry secrets.
	Unlocked() bool
}

// EncryptConfigFiles encrypts the whole default sync set. Kept for
// callers/tests outside a SyncService scope context (the password rotation
// re-encrypts a staging dir rather than going through the store registry).
func EncryptConfigFiles(srcDir, destDir string, key []byte) error {
	return EncryptConfigFilesScoped(defaultSyncedFiles, srcDir, destDir, key)
}

// EncryptConfigFilesScoped encrypts exactly the given config files from
// srcDir into destDir. files must come from EffectiveSyncFiles. The body is
// byte-level: read the plaintext blob, encryptBytes under the sync key,
// write — all field-level semantics live in the stores (ExportPlain /
// ImportPlain), so nothing here knows what a secret field is. A file srcDir
// never carried is skipped: sync must not upload a document that would
// clobber the remote copy.
func EncryptConfigFilesScoped(files []string, srcDir, destDir string, key []byte) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	for _, name := range files {
		plain, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read %s: %w", name, err)
		}
		enc, err := encryptBytes(plain, key)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, name), []byte(enc), 0600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}

// DecryptConfigFiles decrypts the whole default sync set. Kept for
// callers/tests outside a SyncService scope context (the password rotation
// and the wrong-key guard work on staging dirs, not the store registry).
func DecryptConfigFiles(srcDir, destDir string, key []byte) error {
	return DecryptConfigFilesScoped(defaultSyncedFiles, srcDir, destDir, key)
}

// DecryptConfigFilesScoped decrypts exactly the given config files from
// srcDir into destDir. files must come from EffectiveSyncFiles. Byte-level:
// read the encrypted blob, decryptBytes under the sync key, write the
// plaintext — no field handling. A blob the source never carried is
// skipped: sync must not stage an empty document that would disagree with
// the null sentinel hashStoresScoped records for a store whose backing
// file was never written.
func DecryptConfigFilesScoped(files []string, srcDir, destDir string, key []byte) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	for _, name := range files {
		enc, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read %s: %w", name, err)
		}
		plain, err := decryptBytes(string(enc), key)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, name), plain, 0600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}

// encryptBytes encrypts plaintext under key, binding the ciphertext to a
// logical "file" identifier via additional data so an attacker who can
// swap ciphertexts across files (e.g. paste connections.json.enc over
// settings.json.enc) fails the AAD check (SYNC-P1-1).
func encryptBytes(plaintext []byte, key []byte) (string, error) {
	return encryptBytesWithAAD(plaintext, key, nil)
}

func encryptBytesWithAAD(plaintext []byte, key []byte, aad []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, aad)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptBytes(encoded string, key []byte) ([]byte, error) {
	return decryptBytesWithAAD(encoded, key, nil)
}

func decryptBytesWithAAD(encoded string, key []byte, aad []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

// Returns nil if the file doesn't exist (new repo).
func ReadSaltFile(repoPath string) ([]byte, error) {
	saltPath := filepath.Join(repoPath, ".sync-salt")
	data, err := os.ReadFile(saltPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read salt file: %w", err)
	}
	salt, err := hex.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("decode salt: %w", err)
	}
	return salt, nil
}

// WriteSaltFile writes the salt to .sync-salt in the repo directory.
func WriteSaltFile(repoPath string, salt []byte) error {
	saltPath := filepath.Join(repoPath, ".sync-salt")
	return os.WriteFile(saltPath, []byte(hex.EncodeToString(salt)), 0600)
}

// encryptStoresScoped encrypts each store's canonical plaintext blob into
// destDir under the sync key. files must come from EffectiveSyncFiles; a
// store whose blob is not in files is skipped. A store whose backing file
// was never written reports fs.ErrNotExist from ExportPlain — sync skips
// it instead of uploading an empty document that would clobber the remote
// copy.
func encryptStoresScoped(stores []SyncableStore, files []string, destDir string, key []byte) error {
	if err := requireStores(stores, files); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	wanted := make(map[string]bool, len(files))
	for _, name := range files {
		wanted[name] = true
	}

	for _, st := range stores {
		name := st.BlobName()
		if !wanted[name] {
			continue
		}
		plain, err := st.ExportPlain()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("export %s: %w", name, err)
		}
		enc, err := encryptBytes(plain, key)
		if err != nil {
			return fmt.Errorf("encrypt %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, name), []byte(enc), 0600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}

// decryptStoresScoped decrypts exactly the given store blobs from srcDir
// and persists each through its store's ImportPlain, so every save-time
// invariant (password clearing for identity-auth, manifest encryption,
// three hash-skip, quarantine) applies unchanged. files must come from
// EffectiveSyncFiles. A blob the remote never carried is skipped: sync
// must not wipe a local store just because the remote copy is absent.
func decryptStoresScoped(stores []SyncableStore, files []string, srcDir string, key []byte) error {
	if err := requireStores(stores, files); err != nil {
		return err
	}
	wanted := make(map[string]bool, len(files))
	for _, name := range files {
		wanted[name] = true
	}

	for _, st := range stores {
		name := st.BlobName()
		if !wanted[name] {
			continue
		}
		enc, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read %s: %w", name, err)
		}
		plain, err := decryptBytes(string(enc), key)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", name, err)
		}
		if err := st.ImportPlain(plain); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}
	}

	return nil
}
