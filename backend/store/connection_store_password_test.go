package store

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ys-ll/uniterm/backend/session"
)

// enc returns the fakeCipherStore encoding of pw (used to seed encrypted fields).
func enc(pw string) string {
	return "enc:v1:" + base64.StdEncoding.EncodeToString([]byte(pw))
}

func TestConnectionStore_SaveEncryptsPassword(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	data := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: "secret"}},
	}
	if err := s.Save(data); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if contains(string(raw), "secret") {
		t.Fatalf("plaintext leaked: %s", raw)
	}
	if !contains(string(raw), "enc:v1:") {
		t.Fatalf("expected encrypted field on disk, got %s", raw)
	}
}

func TestConnectionStore_LoadDecryptsPassword(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	seed := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: enc("secret")}},
	}
	if err := writeJSONFile(s.filePath(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.Connections[0].Password != "secret" {
		t.Fatalf("Load password = %q, want secret", data.Connections[0].Password)
	}
}

// TestConnectionStore_IdentityKeepsOverrideUser 锁定 issue #959 的语义：
// identity 连接的 User 是用户显式填写的覆盖用户名（非密文），保存时必须
// 原样保留；Password 仍照旧清空，凭据只从密钥库物化。
func TestConnectionStore_IdentityKeepsOverrideUser(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	data := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "identity", IdentityId: "id-1", User: "deploy", Password: enc("stale")}},
	}
	if err := s.Save(data); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !contains(string(raw), "deploy") {
		t.Fatalf("override User lost on save: %s", raw)
	}
	if contains(string(raw), "stale") {
		t.Fatalf("stale Password leaked for identity connection: %s", raw)
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Connections[0].User != "deploy" {
		t.Fatalf("Load User = %q, want deploy", loaded.Connections[0].User)
	}
}

func TestConnectionStore_LoadMigratesLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	seed := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: "legacy"}},
	}
	if err := writeJSONFile(s.filePath(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.Connections[0].Password != "legacy" {
		t.Fatalf("Load should return plaintext, got %q", data.Connections[0].Password)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, storeFileName))
	if !contains(string(raw), "enc:v1:") {
		t.Fatalf("legacy plaintext not migrated to ciphertext: %s", raw)
	}
	if contains(string(raw), "\"password\": \"legacy\"") {
		t.Fatalf("legacy plaintext still on disk: %s", raw)
	}
}

func TestConnectionStore_EnsurePasswordCache(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}
	seed := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: enc("pw")}},
	}
	_ = writeJSONFile(s.filePath(), seed)
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, _ := s.EnsurePassword("c1"); got != "pw" {
		t.Fatalf("EnsurePassword = %q, want pw", got)
	}
	if got, _ := s.EnsurePassword("missing"); got != "" {
		t.Fatalf("EnsurePassword(missing) = %q, want empty", got)
	}
}

// TestConnectionStore_AuthModeSwitchDropsPasswordCache locks in that a cached
// plaintext login password does not survive an auth-mode switch to key/keyText.
// If it did, a later connect of the switched connection would backfill the
// stale password as a key passphrase and break unencrypted private keys.
func TestConnectionStore_AuthModeSwitchDropsPasswordCache(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	// Save as a password connection — this fills the plaintext cache.
	saved := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: "pw"}},
	}
	if err := s.Save(saved); err != nil {
		t.Fatalf("Save password conn: %v", err)
	}
	if got, _ := s.EnsurePassword("c1"); got != "pw" {
		t.Fatalf("EnsurePassword after password save = %q, want pw", got)
	}

	// Switch the same connection to keyText — the cache must be dropped.
	switched := session.ConnectionStoreData{
		Groups: []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{
			ID: "c1", Type: "ssh", AuthType: "keyText",
			KeyContent: "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----",
		}},
	}
	if err := s.Save(switched); err != nil {
		t.Fatalf("Save keyText conn: %v", err)
	}
	if got, _ := s.EnsurePassword("c1"); got != "" {
		t.Fatalf("EnsurePassword after auth-mode switch = %q, want empty", got)
	}
}

// TestConnectionStore_SaveEncryptsKeyText and LoadDecryptsKeyText lock in that
// an inline private-key text (authType "keyText", KeyContent) is encrypted at
// rest in connections.json like a password, so plaintext keys can't leak to disk
// and are decrypted back on load (#720).
func TestConnectionStore_SaveEncryptsKeyText(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	const keyText = "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----"
	data := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "keyText", KeyContent: keyText, Password: "pp"}},
	}
	if err := s.Save(data); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if contains(string(raw), keyText) {
		t.Fatalf("plaintext KeyContent leaked: %s", raw)
	}
	if !contains(string(raw), "enc:v1:") {
		t.Fatalf("expected encrypted KeyContent on disk: %s", raw)
	}
}

func TestConnectionStore_LoadDecryptsKeyText(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	seed := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "keyText", KeyContent: enc("PEM-KEY"), Password: enc("pp")}},
	}
	if err := writeJSONFile(s.filePath(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.Connections[0].KeyContent != "PEM-KEY" {
		t.Fatalf("Load KeyContent = %q, want PEM-KEY", data.Connections[0].KeyContent)
	}
}

// TestConnectionStore_SaveEncryptsKeyPassphrase locks in the field-based
// encryption rule: a key-type connection's private-key passphrase (stored in
// Password) is encrypted at rest exactly like a password-login credential —
// encryption is driven by the secret-field manifest, not by auth type.
func TestConnectionStore_SaveEncryptsKeyPassphrase(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}

	data := session.ConnectionStoreData{
		Groups: []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{
			{ID: "c1", Type: "ssh", AuthType: "key", KeyPath: "/tmp/id_ed25519", Password: "key-passphrase"},
			{ID: "c2", Type: "ssh", AuthType: "keyText", KeyContent: "-----BEGIN OPENSSH PRIVATE KEY-----", Password: "inline-key-passphrase"},
		},
	}
	if err := s.Save(data); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if contains(string(raw), "key-passphrase") ||
		contains(string(raw), "inline-key-passphrase") ||
		contains(string(raw), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("plaintext secret leaked to disk: %s", raw)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Connections[0].Password != "key-passphrase" {
		t.Fatalf("key passphrase not restored: %q", loaded.Connections[0].Password)
	}
	if loaded.Connections[1].KeyContent != "-----BEGIN OPENSSH PRIVATE KEY-----" {
		t.Fatalf("keyContent not restored: %q", loaded.Connections[1].KeyContent)
	}
	if loaded.Connections[1].Password != "inline-key-passphrase" {
		t.Fatalf("keyText passphrase not restored: %q", loaded.Connections[1].Password)
	}
	if got, _ := s.EnsurePassword("c1"); got != "key-passphrase" {
		t.Fatalf("EnsurePassword(c1) = %q, want key-passphrase", got)
	}
}

// refusingCipher accepts any Encrypt but refuses every Decrypt, standing in
// for a wrong or unavailable master key.
type refusingCipher struct{}

func (refusingCipher) Encrypt(p string) (string, error) { return fakeCipherStore{}.Encrypt(p) }
func (refusingCipher) Decrypt(string) (string, error) {
	return "", errors.New("wrong key")
}

// TestConnectionStore_LoadDecryptErrorSurfaces verifies a wrong-key decrypt
// failure propagates from Load instead of silently leaving ciphertext in
// memory (prerequisite for surfacing a re-unlock prompt on migrated data).
func TestConnectionStore_LoadDecryptErrorSurfaces(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}
	if err := s.Save(session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "key", Password: "secret"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.passwordStore = refusingCipher{}
	if _, err := s.Load(); err == nil {
		t.Fatal("expected Load to surface the decrypt error")
	}
}

// TestConnectionStore_SaveSkipsUnchangedContent locks in the F-105 skip:
// the comparison happens on the PRE-encryption input, so saving identical
// data must not touch the file at all (AES-GCM's random nonce would make a
// post-encryption comparison useless).
func TestConnectionStore_SaveSkipsUnchangedContent(t *testing.T) {
	dir := t.TempDir()
	s := &ConnectionStore{configDir: dir, passwordStore: fakeCipherStore{}}
	data := session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: "secret"}},
	}
	if err := s.Save(data); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(dir, storeFileName)
	// Overwrite the on-disk copy with a sentinel: if the next Save rewrites
	// the file, the sentinel disappears and the test fails.
	if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	if err := s.Save(data); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "sentinel" {
		t.Fatal("Save rewrote unchanged content — plaintext-level skip is broken")
	}
	// Changed content must go through.
	changed := data
	changed.Connections = []session.ConnectionConfig{{ID: "c1", Type: "ssh", AuthType: "password", Password: "other"}}
	if err := s.Save(changed); err != nil {
		t.Fatalf("Save changed: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) == "sentinel" {
		t.Fatal("Save skipped content that actually changed")
	}
}
