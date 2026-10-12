package store

import (
	"path/filepath"

	"github.com/ys-ll/uniterm/backend/session"
)

const tunnelsFileName = "tunnels.json"

// TunnelStore persists the user's SSH tunnel definitions to tunnels.json in the
// app config dir, mirroring QuickCommandsStore.
type TunnelStore struct {
	configDir     string
	lastSavedHash string // F-105 skip state (pre-encryption content hash)
}

func NewTunnelStore(configDir string) *TunnelStore {
	return &TunnelStore{configDir: configDir}
}

func (s *TunnelStore) filePath() string {
	return filepath.Join(s.configDir, tunnelsFileName)
}

// Shared Save envelope (filestore.go): hash-skip, encrypt, atomic write.
func (s *TunnelStore) Save(data session.TunnelStoreData) error {
	h, err := saveEncryptedFile(tunnelsFileName, s.filePath(), &data, nil, s.lastSavedHash)
	s.lastSavedHash = h
	return err
}

func (s *TunnelStore) Load() (session.TunnelStoreData, error) {
	out, _, hash, existed, err := loadEncryptedFile[session.TunnelStoreData](tunnelsFileName, s.filePath(), nil)
	if !existed {
		return session.TunnelStoreData{Version: 1, Groups: []session.TunnelGroup{}, Tunnels: []session.Tunnel{}}, nil
	}
	if err != nil {
		return session.TunnelStoreData{}, err
	}
	if out.Version == 0 {
		out.Version = 1
	}
	s.lastSavedHash = hash
	return out, nil
}
