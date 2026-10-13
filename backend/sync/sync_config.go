package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// defaultSyncedFiles is the default sync scope: the config files a device
// syncs before the user picks a scope in the sync settings. The store
// registry (SyncableStore, assembled in app.go), the git commit whitelist
// (git.go), the empty-dir probe
// (isConfigDirEmpty) and the password-rotation file list (ChangePassword)
// all iterate this slice so their scopes can never drift apart again.
//
// It is also the blob-name vocabulary: each syncable store's BlobName is
// one of these, and the sync layer moves a store's canonical plaintext in
// and out without knowing what any file contains.
//
// ai-sessions.json and skills.json are intentionally absent — they are
// local-only data and must never be committed to the sync repo (a stray
// ai-sessions.json left in the repo dir from an old build stays untracked).
//
// settings.json is intentionally absent: it is device-local (theme,
// paths, shells, keybindings, UI state) and changes too often to sync
// well. The syncable AI slice (model catalog + maxTurns) lives in
// ai.json instead; see AIConfigStore.
var defaultSyncedFiles = []string{
	"connections.json",
	"favorites.json",
	"ai.json",
	"quickCommands.json",
	"tunnels.json",
	"identities.json",
	"proxies.json",
}

// syncableFiles is the full menu offered by the sync-scope picker: the
// always-on legacy set plus settings.json. settings.json is the only opt-in
// entry — it is device-leaning (theme, shells, absolute paths, keybindings,
// UI state) and changes too often to sync by default, but a user who wants
// SFTP bookmarks or custom terminal themes on another machine can tick it.
var syncableFiles = append(append([]string{}, defaultSyncedFiles...), "settings.json")

// EffectiveSyncFiles resolves the user's scope against syncableFiles.
// A nil/empty scope returns the default set. Unknown names are dropped so a
// stale scope list from an older or newer build can never smuggle an
// unintended file into the repo.
func EffectiveSyncFiles(scope []string) []string {
	if len(scope) == 0 {
		return append([]string{}, defaultSyncedFiles...)
	}
	valid := make(map[string]bool, len(syncableFiles))
	for _, name := range syncableFiles {
		valid[name] = true
	}
	out := make([]string, 0, len(scope))
	for _, name := range scope {
		if valid[name] {
			delete(valid, name)
			out = append(out, name)
		}
	}
	return out
}

const syncConfigFileName = "sync-config.json"

type SyncConfig struct {
	RepoURL  string `json:"repoUrl"`
	Branch   string `json:"branch"`
	Username string `json:"username"`
	AutoSync bool   `json:"autoSync"`
	// SyncScope is this device's per-file sync selection over syncableFiles.
	// nil/empty resolves to the default set (defaultSyncedFiles).
	SyncScope []string `json:"syncScope,omitempty"`
	// Backend selects the sync transport: "git" (default, private git repo)
	// or "webdav" (snapshot over a WebDAV share).
	Backend      string `json:"backend,omitempty"`
	WebDAVServer string `json:"webdavServer,omitempty"`
	WebDAVPath   string `json:"webdavPath,omitempty"`
	WebDAVUser   string `json:"webdavUser,omitempty"`
	// LastSyncedHash is the aggregate content hash both sides agreed on at
	// the last successful snapshot sync (three-way base for WebDAV mode).
	LastSyncedHash string `json:"lastSyncedHash,omitempty"`

	LastSyncAt     time.Time `json:"lastSyncAt"`
	LastSyncStatus string    `json:"lastSyncStatus"`
	LastSyncError  string    `json:"lastSyncError"`
}

type SyncConfigStore struct {
	configDir string
}

func NewSyncConfigStore(configDir string) *SyncConfigStore {
	return &SyncConfigStore{configDir: configDir}
}

func (s *SyncConfigStore) filePath() string {
	return filepath.Join(s.configDir, syncConfigFileName)
}

func (s *SyncConfigStore) Save(config SyncConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath(), data, 0600)
}

func (s *SyncConfigStore) Load() (SyncConfig, error) {
	data, err := os.ReadFile(s.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return SyncConfig{Branch: "main"}, nil
		}
		return SyncConfig{}, err
	}
	var config SyncConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return SyncConfig{Branch: "main"}, nil
	}
	if config.Branch == "" {
		config.Branch = "main"
	}
	return config, nil
}
