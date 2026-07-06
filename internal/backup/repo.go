// Package backup implements the msgvault backup repository (docs/architecture/backup-format.md):
// an incremental, content-addressed snapshot store built on kit/pack.
package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	// FormatVersion is the backup repository format version this code writes.
	FormatVersion = 1
	// MinReaderVersion is the oldest reader able to read repos this code
	// writes.
	MinReaderVersion = 1
	// SupportedReaderVersion is the newest format this code can read. It is
	// deliberately distinct from FormatVersion ("what we write"): a future
	// release may read formats newer than the one it writes, or vice versa.
	// Repo.Open and LoadManifest refuse anything whose min_reader_version
	// exceeds this.
	SupportedReaderVersion = 2

	// dbPathManifestVersion marks snapshots whose attachment population
	// records storage paths beyond the canonical loose "<aa>/<hash>"
	// derivation. Version-1 readers restored every attachment to the
	// canonical path only, so restoring such a snapshot with one would
	// "succeed" while the database points at files that do not exist; the
	// manifest version bump turns that into an explicit refusal. Snapshots
	// whose paths are all canonical keep version 1 and stay readable by
	// older code.
	dbPathManifestVersion = 2

	repoConfigName   = "config.toml"
	snapshotsDirName = "snapshots"
	packsDirName     = "packs"
	indexesDirName   = "indexes"
	locksDirName     = "locks"
	stagingDirName   = "staging"
	keysDirName      = "keys"
)

// RepoConfig is the plaintext repository descriptor (docs/architecture/backup-format.md). It stays
// unencrypted even in encrypted repos because it bootstraps everything else.
type RepoConfig struct {
	RepoID           string `toml:"repo_id"`
	FormatVersion    int    `toml:"format_version"`
	MinReaderVersion int    `toml:"min_reader_version"`
	Encryption       string `toml:"encryption"`
	CreatedAt        string `toml:"created_at"`
	PageSize         int    `toml:"page_size"`
}

// Repo is an opened backup repository rooted at a directory.
type Repo struct {
	root string
	cfg  RepoConfig
}

// Init creates a new empty repository at root. It refuses to reuse a
// directory that already contains a repository config.
func Init(root string) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(root, repoConfigName)); err == nil {
		return nil, fmt.Errorf("backup: repository already initialized at %s",
			root)
	}
	for _, dir := range []string{
		root,
		filepath.Join(root, snapshotsDirName),
		filepath.Join(root, packsDirName),
		filepath.Join(root, indexesDirName),
		filepath.Join(root, locksDirName),
		filepath.Join(root, stagingDirName),
		filepath.Join(root, keysDirName),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("backup: creating %s: %w", dir, err)
		}
	}
	cfg := RepoConfig{
		RepoID:           newRepoID(),
		FormatVersion:    FormatVersion,
		MinReaderVersion: MinReaderVersion,
		Encryption:       "none",
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	r := &Repo{root: root, cfg: cfg}
	if err := r.writeConfig(); err != nil {
		return nil, err
	}
	return r, nil
}

// Open loads an existing repository and enforces version compatibility.
func Open(root string) (*Repo, error) {
	data, err := os.ReadFile(filepath.Join(root, repoConfigName))
	if err != nil {
		return nil, fmt.Errorf("backup: opening repository at %s: %w", root,
			err)
	}
	var cfg RepoConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("backup: parsing repository config: %w", err)
	}
	if cfg.MinReaderVersion > SupportedReaderVersion {
		return nil, fmt.Errorf(
			"backup: repository requires reader version %d but this "+
				"msgvault supports %d; upgrade msgvault",
			cfg.MinReaderVersion, SupportedReaderVersion)
	}
	if cfg.Encryption != "none" {
		return nil, fmt.Errorf(
			"backup: encrypted repositories are not supported yet "+
				"(encryption=%q)", cfg.Encryption)
	}
	return &Repo{root: root, cfg: cfg}, nil
}

// Root returns the repository root directory.
func (r *Repo) Root() string { return r.root }

// Config returns the repository descriptor.
func (r *Repo) Config() RepoConfig { return r.cfg }

// Path joins parts under the repository root.
func (r *Repo) Path(parts ...string) string {
	return filepath.Join(append([]string{r.root}, parts...)...)
}

// SetPageSize records the DB page size after the first backup.
func (r *Repo) SetPageSize(pageSize int) error {
	if r.cfg.PageSize == pageSize {
		return nil
	}
	r.cfg.PageSize = pageSize
	return r.writeConfig()
}

func (r *Repo) writeConfig() error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(r.cfg); err != nil {
		return fmt.Errorf("backup: encoding repository config: %w", err)
	}
	return writeFileAtomic(r, repoConfigName, buf.Bytes())
}

// CleanStaging removes in-flight write debris. Callers must hold the
// exclusive repo lock (concurrent writers stage under the same directory).
func (r *Repo) CleanStaging() error {
	staging := r.Path(stagingDirName)
	info, err := os.Lstat(staging)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.MkdirAll(staging, 0o700)
	case err != nil:
		return fmt.Errorf("backup: checking staging dir: %w", err)
	case !info.IsDir():
		// Lstat never follows symlinks, so a "staging" symlink planted by
		// another principal reports ModeSymlink here instead of the
		// target's mode. Refuse rather than RemoveAll entries wherever it
		// points.
		return fmt.Errorf("backup: staging path %s is not a directory; refusing to clean it", staging)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("backup: reading staging dir: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(r.Path(stagingDirName, e.Name())); err != nil {
			return fmt.Errorf("backup: cleaning staging entry %s: %w",
				e.Name(), err)
		}
	}
	return nil
}

func newRepoID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("backup: reading random bytes: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10],
		b[10:16])
}
