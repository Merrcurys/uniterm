package store

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/ys-ll/uniterm/backend/session"
)

const identityStoreFileName = "identities.json"

type IdentityStore struct {
	configDir     string
	passwordStore PasswordStore // nil = refuse to write plaintext passwords
	mu            sync.Mutex    // serializes Save + Load password rewrites
	lastSavedHash string        // F-105 skip state (pre-encryption content hash)
}

func NewIdentityStore(configDir string) (*IdentityStore, error) {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return nil, err
	}
	return &IdentityStore{configDir: configDir}, nil
}

// SetPasswordStore sets the external password store used to encrypt the
// Password field on save and decrypt it on load.
func (s *IdentityStore) SetPasswordStore(ps PasswordStore) {
	s.passwordStore = ps
}

func (s *IdentityStore) filePath() string {
	return filepath.Join(s.configDir, identityStoreFileName)
}

// Save writes data to identities.json, encrypting only the Password field.
// It fails closed rather than persisting a plaintext password when no
// passwordStore is wired. Values already prefixed (credentials.IsEncrypted)
// are skipped so a save doesn't double-encrypt previously encrypted data.
func (s *IdentityStore) Save(data session.IdentityStoreData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Deep-copy identities so we don't mutate the caller's backing array.
	identities := make([]session.Identity, len(data.Identities))
	copy(identities, data.Identities)

	out := session.IdentityStoreData{Identities: identities}
	// Shared Save envelope (filestore.go): hash-skip, encrypt, atomic write.
	h, err := saveEncryptedFile(identityStoreFileName, s.filePath(), &out, s.passwordStore, s.lastSavedHash)
	s.lastSavedHash = h
	return err
}

// Load reads identities.json: empty store when the file does not exist,
// quarantine+error on corrupt JSON, decrypting the manifest secret fields
// (filestore.go envelope).
func (s *IdentityStore) Load() (session.IdentityStoreData, error) {
	out, _, hash, existed, err := loadEncryptedFile[session.IdentityStoreData](identityStoreFileName, s.filePath(), s.passwordStore)
	if !existed {
		return session.IdentityStoreData{Identities: []session.Identity{}}, nil
	}
	if err != nil {
		return session.IdentityStoreData{}, err
	}
	if out.Identities == nil {
		out.Identities = []session.Identity{}
	}
	s.lastSavedHash = hash
	return out, nil
}
