package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/ys-ll/uniterm/backend/credentials"
	"github.com/ys-ll/uniterm/backend/session"
)

const storeFileName = "connections.json"

// PasswordStore encrypts/decrypts individual secret field values in-place.
// Implementations own the master key (OS keychain or master-password derived).
type PasswordStore interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(encoded string) (string, error)
}

type ConnectionStore struct {
	configDir     string
	passwordStore PasswordStore        // nil = encrypted fields pass through untouched (backward compat)
	legacy        LegacyPasswordSource // optional fallback to pre-enc:v1 keychain entries
	mu            sync.Mutex           // serializes Save + Load lazy re-encrypts (STORE-05/06).
	pwdMu         sync.RWMutex         // guards pwdCache for F-110 async keychain fill.
	pwdCache      map[string]string
	lastSavedHash string // F-105: skip no-op rewrites keyed by canonical content hash.
}

func NewConnectionStore(configDir string) (*ConnectionStore, error) {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return nil, err
	}
	return &ConnectionStore{configDir: configDir}, nil
}

// SetPasswordStore sets the external password store. Once set, secret fields
// are written encrypted and decrypted on load.
func (s *ConnectionStore) SetPasswordStore(ps PasswordStore) {
	s.passwordStore = ps
}

// SetLegacyKeychain wires the pre-enc:v1 keychain so a connection that lacks
// an enc:v1 field can still recover its password from the legacy conn/<id>
// entry. Lazy-migrates the value into enc:v1 on the next save.
func (s *ConnectionStore) SetLegacyKeychain(kc LegacyPasswordSource) {
	s.legacy = kc
}

func (s *ConnectionStore) filePath() string {
	return filepath.Join(s.configDir, storeFileName)
}

func (s *ConnectionStore) Save(data session.ConnectionStoreData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Deep-copy connections so we don't mutate the caller's backing array
	connections := make([]session.ConnectionConfig, len(data.Connections))
	copy(connections, data.Connections)

	for i := range connections {
		conn := &connections[i]
		if conn.AuthType == "identity" {
			// Identity connections obtain credentials solely from the
			// referenced identity (MaterializeIdentity resolves them at
			// connect time). A stale password left when switching away from
			// another auth mode is unused — and, if it still holds an
			// enc:v1: field, would be carried verbatim into cloud sync,
			// surfacing as a literal enc:v1: on other devices (issue #711).
			// User, however, is the explicit username override typed into
			// the connection form (issue #959) — plain text by definition,
			// so it is kept as-is.
			conn.Password = ""
		}
		// The plaintext cache mirrors the saved password field for every
		// auth type — key/keyText connections keep their private-key
		// passphrase there (session/ssh_auth.go), so it is as much a
		// credential as a password-login password. Cleared when the field is
		// emptied so EnsurePassword can't resurrect a deleted password.
		s.pwdMu.Lock()
		if conn.Password == "" {
			delete(s.pwdCache, conn.ID)
		} else if !credentials.IsEncrypted(conn.Password) {
			if s.pwdCache == nil {
				s.pwdCache = map[string]string{}
			}
			s.pwdCache[conn.ID] = conn.Password
		}
		s.pwdMu.Unlock()
	}

	saveData := session.ConnectionStoreData{
		Groups:      data.Groups,
		Connections: connections,
	}
	// Shared Save envelope (filestore.go): hash-skip, encrypt, atomic write.
	h, err := saveEncryptedFile(storeFileName, s.filePath(), &saveData, s.passwordStore, s.lastSavedHash)
	s.lastSavedHash = h
	if errors.Is(err, ErrNoCipher) {
		return errors.New("passwordStore not initialized; refusing to save plaintext password")
	}
	return err
}

func (s *ConnectionStore) Load() (session.ConnectionStoreData, error) {
	raw, err := os.ReadFile(s.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return session.ConnectionStoreData{
				Groups:      []session.ConnectionGroup{},
				Connections: []session.ConnectionConfig{},
			}, nil
		}
		return session.ConnectionStoreData{}, err
	}

	// Try new format first: {"groups": [...], "connections": [...]}
	data, res, err := loadEncryptedData[session.ConnectionStoreData](storeFileName, raw, s.passwordStore)
	if err == nil && (data.Groups != nil || data.Connections != nil) {
		return s.finishLoaded(&data, res), nil
	}
	if err != nil && !errors.Is(err, errUnparseableConfig) {
		// Parsed as the new shape but the fields wouldn't decrypt — the file
		// itself is intact, surface the error instead of quarantining.
		return session.ConnectionStoreData{}, err
	}

	// Fallback: old format — plain array of connections
	connections, res, err := loadEncryptedData[[]session.ConnectionConfig](storeFileName, raw, s.passwordStore)
	if err != nil {
		if errors.Is(err, errUnparseableConfig) {
			// STORE-09: rename corrupt JSON aside before re-attempting.
			quarantineCorrupt(s.filePath())
		}
		return session.ConnectionStoreData{}, err
	}
	return s.finishLoaded(&session.ConnectionStoreData{
		Groups:      []session.ConnectionGroup{},
		Connections: connections,
	}, res), nil
}

// finishLoaded normalizes nil slices and applies the load-time policies the
// manifest walk can't express: the legacy keychain backfill, the plaintext
// cache used by EnsurePassword, and the lazy re-encrypt of legacy plaintext
// (and orphaned enc:v1 residue left by older auth-type-conditional builds).
func (s *ConnectionStore) finishLoaded(data *session.ConnectionStoreData, res DecryptResult) session.ConnectionStoreData {
	if data.Groups == nil {
		data.Groups = []session.ConnectionGroup{}
	}
	if data.Connections == nil {
		data.Connections = []session.ConnectionConfig{}
	}
	needsSave := res.Plaintext > 0

	for i := range data.Connections {
		conn := &data.Connections[i]
		if conn.AuthType == "password" && conn.Password == "" && s.legacy != nil {
			// Pre-enc:v1 scheme: the password lived in the keychain under
			// conn/<id> and the JSON field was omitted; recover it here so
			// the value stays visible without a manual re-entry. Password
			// auth only — key/keyText passphrases never went through the
			// legacy keychain, and backfilling them would break unencrypted
			// keys. Falls back to "" when neither source has it.
			if pw, err := s.legacy.GetPassword(conn.ID); err == nil && pw != "" {
				conn.Password = pw
				s.cachePwd(conn.ID, pw)
				needsSave = true // lazy-migrate into enc:v1 on next save
			}
			continue
		}
		if conn.Password != "" && !credentials.IsEncrypted(conn.Password) {
			s.cachePwd(conn.ID, conn.Password)
		}
	}

	if needsSave {
		clean := *data
		clean.Connections = make([]session.ConnectionConfig, len(data.Connections))
		copy(clean.Connections, data.Connections)
		// Best-effort lazy re-encrypt: a missing cipher or an encrypt error
		// leaves the file untouched; the plaintext re-encrypts on next Save.
		// forceWrite bypasses the hash-skip: the plaintext content is
		// unchanged on disk, so the seeded hash would match and the lazy
		// encryption would never run (docs 4.3). On error the envelope
		// returns "" so the next Save retries the re-encrypt.
		h, _ := saveEncryptedFile(storeFileName, s.filePath(), &clean, s.passwordStore, forceWrite)
		s.mu.Lock()
		s.lastSavedHash = h
		s.mu.Unlock()
	} else {
		// Seed the skip-state from the just-loaded content so an unchanged
		// first Save after startup skips the rewrite.
		if hashHex, err := jsonHash(*data); err == nil {
			s.mu.Lock()
			s.lastSavedHash = hashHex
			s.mu.Unlock()
		}
	}
	return *data
}

// cachePwd stores a decrypted password in the EnsurePassword cache.
func (s *ConnectionStore) cachePwd(id, pw string) {
	s.pwdMu.Lock()
	if s.pwdCache == nil {
		s.pwdCache = map[string]string{}
	}
	s.pwdCache[id] = pw
	s.pwdMu.Unlock()
}

// EnsurePassword returns the cached decrypted password for a connection ID.
// Load() fills the cache synchronously from encrypted fields, so this is a
// defensive fallback for callers that construct configs without going through
// Load. On a cache miss it tries the legacy keychain (conn/<id>) so a connect
// attempt can still recover a password that was never migrated into enc:v1.
// Returns "" when there is no cached entry and no legacy keychain value.
func (s *ConnectionStore) EnsurePassword(connID string) (string, error) {
	s.pwdMu.RLock()
	pw, ok := s.pwdCache[connID]
	s.pwdMu.RUnlock()
	if ok {
		return pw, nil
	}
	if s.legacy == nil {
		return "", nil
	}
	pw, err := s.legacy.GetPassword(connID)
	if err != nil {
		return "", nil
	}
	if pw != "" {
		s.cachePwd(connID, pw)
	}
	return pw, nil
}
