package credentials

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	unitsync "github.com/ys-ll/uniterm/backend/sync"
)

type fakeKeychain struct {
	mu    sync.Mutex
	store map[string]string
}

func newFakeKeychain() *fakeKeychain { return &fakeKeychain{store: map[string]string{}} }
func (f *fakeKeychain) Get(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.store[key]; ok {
		return v, nil
	}
	return "", ErrKeychainNotFound
}
func (f *fakeKeychain) Set(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[key] = value
	return nil
}
func (f *fakeKeychain) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, key)
	return nil
}

// transientKeychain reports a non-notfound error on reads, simulating e.g. a
// temporarily denied macOS keychain permission.
type transientKeychain struct{}

func (transientKeychain) Get(string) (string, error) {
	return "", errors.New("keychain temporarily unavailable")
}
func (transientKeychain) Set(string, string) error { return nil }
func (transientKeychain) Delete(string) error      { return nil }

func TestSetupKeychainMode(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	s := New(dir, kc)
	if err := s.Setup(ModeKeychain, ""); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	st := s.Status()
	if st.Mode != ModeKeychain || !st.Unlocked || st.NeedsSetup || st.KeychainLost {
		t.Fatalf("status = %+v", st)
	}
	// Round-trip encrypt/decrypt.
	enc, err := s.Encrypt("pw")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := s.Decrypt(enc)
	if err != nil || got != "pw" {
		t.Fatalf("Decrypt = %q, %v", got, err)
	}
	// Key persisted to keychain-key/<hash>.
	if len(kc.store["keychain-key/"+s.DirHash()]) == 0 {
		t.Fatalf("keychain key not persisted")
	}
}

func TestSetupMasterPasswordThenUnlock(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	s := New(dir, kc)
	if err := s.Setup(ModeMasterPassword, "hunter2"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// A fresh store (simulating restart) must unlock from cached key.
	s2 := New(dir, kc)
	if err := s2.AutoUnlock(); err != nil {
		t.Fatalf("AutoUnlock: %v", err)
	}
	if !s2.Status().Unlocked {
		t.Fatalf("expected auto-unlock from cached key")
	}
	// After losing the cache, Unlock with the password re-derives the key.
	s3 := New(dir, kc)
	_ = kc.Delete("master-key/" + s3.DirHash())
	if err := s3.AutoUnlock(); err != nil {
		t.Fatalf("AutoUnlock: %v", err)
	}
	if s3.Status().Unlocked {
		t.Fatalf("expected locked after cache cleared")
	}
	if err := s3.Unlock("hunter2"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if !s3.Status().Unlocked {
		t.Fatalf("expected unlocked after Unlock")
	}
}

func TestAutoUnlockKeychainLost(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	s := New(dir, kc)
	_ = s.Setup(ModeKeychain, "")
	// Simulate a fresh machine: meta exists but keychain key is gone.
	_ = kc.Delete("keychain-key/" + s.DirHash())
	s2 := New(dir, kc)
	if err := s2.AutoUnlock(); err != nil {
		t.Fatalf("AutoUnlock: %v", err)
	}
	if !s2.Status().KeychainLost {
		t.Fatalf("expected KeychainLost, got %+v", s2.Status())
	}
}

func TestEncryptWhenLockedFails(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	s := New(dir, kc)
	_ = s.Setup(ModeMasterPassword, "pw")
	_ = kc.Delete("master-key/" + s.DirHash())
	s2 := New(dir, kc)
	_ = s2.AutoUnlock() // locked
	if _, err := s2.Encrypt("x"); err == nil {
		t.Fatalf("Encrypt should fail when locked")
	}
}

// TestStoreHasSecretsUsesWiredFileList: the file list that may carry in-place
// ciphertext is wired by the app from the store's secret-field manifest (it
// cannot be declared here — store imports credentials). An unwired store
// reports nothing.
func TestStoreHasSecretsUsesWiredFileList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "connections.json"),
		[]byte(`{"connections":[{"password":"enc:v1:x"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai.json"),
		[]byte(`{"models":[{"apiKey":"enc:v1:y"}]}`), 0600); err != nil {
		t.Fatal(err)
	}

	s := New(dir, newFakeKeychain())
	if s.storeHasSecrets() {
		t.Fatal("an unwired store must not scan for ciphertext")
	}

	s.SetSecretFiles([]string{"connections.json", "ai.json"})
	if !s.storeHasSecrets() {
		t.Fatal("ciphertext in a wired file must be detected")
	}
}

// --- P1: a cached key that cannot decrypt the data must not pass as
// "unlocked" — the state must surface so the user is asked for the master
// password (or offered clearing), exactly like opening after a migration. ---

// seedCiphertext writes a connection file whose password field is encrypted
// under key, and claims master-password mode with the given salt.
func seedCiphertext(t *testing.T, dir string, key []byte, mode string, salt []byte) {
	t.Helper()
	enc, err := EncryptField("secret", key)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"connections":[{"id":"c1","password":"` + enc + `"}]}`
	if mode == ModeKeychain {
		body = `{"connections":[{"id":"c1","password":"` + enc + `"}]}`
	}
	if err := os.WriteFile(filepath.Join(dir, "connections.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, &Meta{Mode: mode, Salt: salt}); err != nil {
		t.Fatal(err)
	}
}

// TestAutoUnlockStaleCacheKeyDecryptFailed: a cached derived key left over
// from a previous setup cannot open the current data. AutoUnlock must refuse
// to report success, and the right master password must recover — repairing
// the poisoned cache on the way.
func TestAutoUnlockStaleCacheKeyDecryptFailed(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	salt := []byte("0123456789abcdef")
	seedCiphertext(t, dir, unitsync.DeriveKey("right", salt), ModeMasterPassword, salt)

	h := New(dir, kc).DirHash()
	if err := kc.Set("master-key/"+h, hex.EncodeToString(unitsync.DeriveKey("old", salt))); err != nil {
		t.Fatal(err)
	}

	s := New(dir, kc)
	s.SetSecretFiles([]string{"connections.json"})
	if err := s.AutoUnlock(); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.Unlocked || !st.DecryptFailed {
		t.Fatalf("stale cache key must not unlock: %+v", st)
	}

	// The right master password recovers, and the poisoned cache is repaired.
	if err := s.Unlock("right"); err != nil {
		t.Fatalf("unlock with the right password: %v", err)
	}
	if !s.Status().Unlocked {
		t.Fatal("the right password must unlock")
	}
	s2 := New(dir, kc)
	s2.SetSecretFiles([]string{"connections.json"})
	if err := s2.AutoUnlock(); err != nil {
		t.Fatal(err)
	}
	if st := s2.Status(); !st.Unlocked || st.DecryptFailed {
		t.Fatalf("cache not repaired by the successful unlock: %+v", s2.Status())
	}
}

// TestUnlockWrongPasswordKeepsCache: a failed unlock must not overwrite the
// cached entry — the old behaviour cached the wrong attempt's key first and
// poisoned a working state.
func TestUnlockWrongPasswordKeepsCache(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	salt := []byte("0123456789abcdef")
	seedCiphertext(t, dir, unitsync.DeriveKey("right", salt), ModeMasterPassword, salt)

	h := New(dir, kc).DirHash()
	stale := hex.EncodeToString(unitsync.DeriveKey("old", salt))
	if err := kc.Set("master-key/"+h, stale); err != nil {
		t.Fatal(err)
	}

	s := New(dir, kc)
	s.SetSecretFiles([]string{"connections.json"})
	if err := s.Unlock("wrong"); err == nil {
		t.Fatal("a wrong master password must fail")
	}
	if v, err := kc.Get("master-key/" + h); err != nil || v != stale {
		t.Fatalf("failed unlock must not touch the cache: %q err=%v", v, err)
	}
}

// TestAutoUnlockStaleKeychainKeyDecryptFailed: the keychain entry exists but
// holds THIS machine's key while the data was encrypted elsewhere — the state
// is the same failed-verification one, not a lost key (the entry is right
// there), and a master password cannot fix it in this mode.
func TestAutoUnlockStaleKeychainKeyDecryptFailed(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	salt := []byte("0123456789abcdef")
	seedCiphertext(t, dir, unitsync.DeriveKey("other-machine", salt), ModeKeychain, salt)

	h := New(dir, kc).DirHash()
	if err := kc.Set("keychain-key/"+h, hex.EncodeToString(unitsync.DeriveKey("this-machine", salt))); err != nil {
		t.Fatal(err)
	}

	s := New(dir, kc)
	s.SetSecretFiles([]string{"connections.json"})
	if err := s.AutoUnlock(); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.Unlocked || !st.DecryptFailed {
		t.Fatalf("a key that cannot decrypt must not unlock: %+v", st)
	}
	if st.KeychainLost {
		t.Fatal("the key entry exists — this is not a lost key")
	}
	if err := s.Unlock("anything"); err == nil {
		t.Fatal("keychain mode cannot be unlocked with a password")
	}
}

// TestAutoUnlockWithoutCiphertextTriviallyUnlocks: with no ciphertext in the
// wired files there is nothing a wrong key could break — verification passes
// and behaviour is unchanged (fresh installs, post-reset dirs).
func TestAutoUnlockWithoutCiphertextTriviallyUnlocks(t *testing.T) {
	dir := t.TempDir()
	kc := newFakeKeychain()
	salt := []byte("0123456789abcdef")
	if err := WriteMeta(dir, &Meta{Mode: ModeMasterPassword, Salt: salt}); err != nil {
		t.Fatal(err)
	}
	h := New(dir, kc).DirHash()
	if err := kc.Set("master-key/"+h, hex.EncodeToString(unitsync.DeriveKey("whatever", salt))); err != nil {
		t.Fatal(err)
	}

	s := New(dir, kc)
	if err := s.AutoUnlock(); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); !st.Unlocked || st.DecryptFailed {
		t.Fatalf("a dir without ciphertext must unlock trivially: %+v", st)
	}
}
