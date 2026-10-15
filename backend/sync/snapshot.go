package sync

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	manifestName = "manifest.json"
	saltName     = ".sync-salt"
)

// remoteSnapshotBackend is the unified snapshot engine: it implements
// SnapshotBackend over any RemoteFS, so webdav, sftp, s3 and smb share one
// implementation of the snapshot semantics (remote layout, manifest last
// as commit point, salt hex encoding).
//
// The remote layout is unchanged from the original WebDAV backend:
// basePath/.sync-salt (hex), one ciphertext resource per scoped config
// file, and manifest.json written LAST as the commit point — readers
// never observe a half-written snapshot.
type remoteSnapshotBackend struct {
	fs    RemoteFS
	files []string
}

// NewRemoteSnapshotBackend builds the engine over fs. files is the scoped
// file list used by StoreAll's staging upload.
func NewRemoteSnapshotBackend(fs RemoteFS, files []string) SnapshotBackend {
	return &remoteSnapshotBackend{fs: fs, files: append([]string{}, files...)}
}

// Probe verifies the share is reachable and creates the collection.
func (b *remoteSnapshotBackend) Probe() error {
	return b.fs.Probe()
}

// Head returns the remote manifest, or nil when none exists yet (fresh
// remote). ErrRemoteNotFound maps to the nil case.
func (b *remoteSnapshotBackend) Head() (*SnapshotInfo, error) {
	data, err := b.fs.Get(manifestName)
	if err != nil {
		if errors.Is(err, ErrRemoteNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var info SnapshotInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &info, nil
}

// FetchAll mirrors the remote snapshot into dir. A missing manifest means
// an empty remote, which is a no-op.
func (b *remoteSnapshotBackend) FetchAll(dir string) error {
	info, err := b.Head()
	if err != nil {
		return err
	}
	if info == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if salt, err := b.fs.Get(saltName); err == nil {
		raw, err := hex.DecodeString(string(salt))
		if err != nil {
			return fmt.Errorf("decode %s: %w", saltName, err)
		}
		if err := WriteSaltFile(dir, raw); err != nil {
			return err
		}
	} else if !errors.Is(err, ErrRemoteNotFound) {
		return err
	}
	nullHash := nullStoreHash()
	for name, h := range info.FileHashes {
		if h == nullHash {
			// A never-persisted store: the manifest records the null
			// hash so aggregate hashes stay comparable, but the remote
			// carries no blob for it.
			continue
		}
		data, err := b.fs.Get(name)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, manifestName), mustJSON(info), 0600)
}

// StoreAll uploads ciphertext files from stagingDir (named by
// manifest.FileHashes), the salt, then the manifest last.
func (b *remoteSnapshotBackend) StoreAll(stagingDir string, salt []byte, manifest SnapshotInfo) error {
	// Upload only the blobs actually staged: the caller stages the delta
	// (the blobs whose plaintext changed), so the remote keeps its copy of
	// every unchanged blob under the same key. The manifest still
	// describes the whole snapshot.
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return fmt.Errorf("read staging: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == saltName {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stagingDir, e.Name()))
		if err != nil {
			return fmt.Errorf("stage %s: %w", e.Name(), err)
		}
		if err := b.fs.Put(e.Name(), data); err != nil {
			return err
		}
	}
	// The salt is stored hex-encoded on the remote, matching
	// WriteSaltFile/ReadSaltFile in git mode.
	if err := b.fs.Put(saltName, []byte(hex.EncodeToString(salt))); err != nil {
		return err
	}
	return b.fs.Put(manifestName, mustJSON(manifest))
}

func mustJSON(v interface{}) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // SnapshotInfo only contains serializable fields
	}
	return data
}
