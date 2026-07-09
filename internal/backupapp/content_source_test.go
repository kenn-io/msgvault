package backupapp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/store"
)

// vaultFixture is a small SQLite-backed archive: a real store with one
// message plus helpers to record attachment rows with matching loose blob
// files under a temp attachments dir. SQLite is intrinsic here — the kit
// backup engine snapshots a SQLite database file.
type vaultFixture struct {
	t       *testing.T
	store   *store.Store
	dbPath  string
	dataDir string
	attDir  string
	msgID   int64
	seq     int
}

func newVaultFixture(t *testing.T) *vaultFixture {
	t.Helper()
	require := require.New(t)
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-cs", "Content Source Thread")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "cs-msg",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage")
	return &vaultFixture{
		t:       t,
		store:   st,
		dbPath:  dbPath,
		dataDir: dataDir,
		attDir:  filepath.Join(dataDir, "attachments"),
		msgID:   msgID,
	}
}

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func canonicalPath(hash string) string { return hash[:2] + "/" + hash }

// addBlob records an attachment row at relPath and writes the matching loose
// file, returning the content hash.
func (f *vaultFixture) addBlob(content []byte, relPath string) string {
	f.t.Helper()
	h := hashOf(content)
	f.seq++
	err := f.store.UpsertAttachment(f.msgID, fmt.Sprintf("file-%d.bin", f.seq),
		"application/octet-stream", relPath, h, len(content))
	require.NoErrorf(f.t, err, "UpsertAttachment(%s)", h)
	f.writeLoose(relPath, content)
	return h
}

// writeLoose writes content at the slash-separated relPath under the
// attachments dir.
func (f *vaultFixture) writeLoose(relPath string, content []byte) {
	f.t.Helper()
	full := filepath.Join(f.attDir, filepath.FromSlash(relPath))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o700), "mkdir loose dir")
	require.NoError(f.t, os.WriteFile(full, content, 0o600), "write loose file")
}

// contentSource builds the production blob store over the fixture and wraps
// it in the backup ContentSource under test.
func (f *vaultFixture) contentSource() backup.ContentSource {
	f.t.Helper()
	blobs := blobstore.New(f.store, f.attDir)
	f.t.Cleanup(func() { _ = blobs.Close() })
	return backupapp.NewContentSource(blobs, f.attDir)
}

// pack runs the packer over the fixture vault.
func (f *vaultFixture) pack() packer.Stats {
	f.t.Helper()
	stats, err := packer.Run(context.Background(), f.store, f.attDir, packer.Options{})
	require.NoError(f.t, err, "packer.Run")
	return stats
}

// looseFiles returns every regular file under the attachments dir outside the
// packs subtree, relative to the attachments dir.
func (f *vaultFixture) looseFiles() []string {
	f.t.Helper()
	var files []string
	err := filepath.WalkDir(f.attDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "packs" && filepath.Dir(path) == f.attDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(f.attDir, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, rel)
		return nil
	})
	require.NoError(f.t, err, "walk attachments dir")
	return files
}

func readAllAndClose(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	data, err := io.ReadAll(rc)
	require.NoError(t, err, "read blob")
	require.NoError(t, rc.Close(), "close blob")
	return data
}

func TestContentSourcePackedBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)

	content := []byte("packed attachment payload")
	h := f.addBlob(content, canonicalPath(hashOf(content)))
	stats := f.pack()
	require.Equal(1, stats.BlobsPacked)
	require.Empty(f.looseFiles(), "packer must leave no loose content")

	src := f.contentSource()
	rc, err := src.Open(context.Background(), backup.ContentRef{
		Hash: h, Size: int64(len(content)), StoragePath: canonicalPath(h),
	})
	require.NoError(err)
	assert.Equal(content, readAllAndClose(t, rc))
}

func TestContentSourceCanonicalLooseBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)

	content := []byte("loose canonical payload")
	h := f.addBlob(content, canonicalPath(hashOf(content)))

	rc, err := f.contentSource().Open(context.Background(), backup.ContentRef{
		Hash: h, Size: int64(len(content)), StoragePath: canonicalPath(h),
	})
	require.NoError(err)
	assert.Equal(content, readAllAndClose(t, rc))
}

func TestContentSourceNoncanonicalFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)

	content := []byte("legacy synctech payload")
	h := hashOf(content)
	rel := "synctech-sms/" + h[:2] + "/" + h
	f.addBlob(content, rel) // recorded at the legacy path, never indexed

	rc, err := f.contentSource().Open(context.Background(), backup.ContentRef{
		Hash: h, Size: int64(len(content)), StoragePath: rel,
	})
	require.NoError(err)
	assert.Equal(content, readAllAndClose(t, rc))
}

func TestContentSourceMissingBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	src := f.contentSource()
	h := hashOf([]byte("never stored anywhere"))

	// Canonical StoragePath: nothing more to try beyond the blob store.
	_, err := src.Open(context.Background(), backup.ContentRef{
		Hash: h, Size: 1, StoragePath: canonicalPath(h),
	})
	require.ErrorIs(err, fs.ErrNotExist)
	assert.Contains(err.Error(), h)

	// Recorded noncanonical path that is also missing.
	_, err = src.Open(context.Background(), backup.ContentRef{
		Hash: h, Size: 1, StoragePath: "synctech-sms/" + h[:2] + "/" + h,
	})
	require.Error(err)
	assert.Contains(err.Error(), h)
	assert.Contains(err.Error(), "synctech-sms/")
}

func TestContentSourceMalformedHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	src := f.contentSource()

	for _, hash := range []string{"", "zz", "not-a-hash"} {
		var err error
		assert.NotPanicsf(func() {
			// A non-empty StoragePath would reach the hash-slicing fallback
			// if the validation error ever satisfied fs.ErrNotExist.
			_, err = src.Open(context.Background(), backup.ContentRef{
				Hash: hash, Size: 1, StoragePath: "synctech-sms/xx/" + hash,
			})
		}, "hash %q", hash)
		require.ErrorIsf(err, export.ErrInvalidContentHash, "hash %q", hash)
		assert.NotErrorIsf(err, fs.ErrNotExist, "hash %q", hash)
	}
}

func TestContentSourceNonLocalStoragePath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	h := hashOf([]byte("escaping blob"))

	_, err := f.contentSource().Open(context.Background(), backup.ContentRef{
		Hash: h, Size: 1, StoragePath: "../evil",
	})
	require.Error(err)
	assert.Contains(err.Error(), "non-local storage path")
	assert.Contains(err.Error(), h)
}

func TestContentSourceCancelledContext(t *testing.T) {
	assert := assert.New(t)
	f := newVaultFixture(t)
	content := []byte("cancelled read payload")
	h := f.addBlob(content, canonicalPath(hashOf(content)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.contentSource().Open(ctx, backup.ContentRef{
		Hash: h, Size: int64(len(content)), StoragePath: canonicalPath(h),
	})
	assert.ErrorIs(err, context.Canceled)
}

// TestBackupCreatePackedVaultEndToEnd proves capture never needs loose files:
// a fully packed vault (one canonical blob, one legacy noncanonical blob,
// zero loose content) round-trips through create → verify → restore with the
// ContentSource supplying every attachment byte.
func TestBackupCreatePackedVaultEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	ctx := context.Background()

	contentA := []byte("first packed attachment")
	contentB := []byte("second attachment, imported at a legacy path")
	hashA := f.addBlob(contentA, canonicalPath(hashOf(contentA)))
	hashB := hashOf(contentB)
	f.addBlob(contentB, "synctech-sms/"+hashB[:2]+"/"+hashB)

	stats := f.pack()
	require.Equal(2, stats.BlobsPacked)
	require.Empty(f.looseFiles(), "packer must leave no loose content")

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err, "backup.Init")
	app := backupapp.New("test")
	m, err := backup.Create(ctx, repo, app, backup.CreateOptions{
		DBPath:        f.dbPath,
		ContentDir:    f.attDir,
		DataDir:       f.dataDir,
		ContentSource: f.contentSource(),
	})
	require.NoError(err, "backup.Create")
	assert.Equal(int64(2), m.Attachments.Blobs)

	verifyRes, err := backup.Verify(ctx, repo, app, backup.VerifyOptions{All: true})
	require.NoError(err, "backup.Verify")
	assert.Empty(verifyRes.Problems)

	target := filepath.Join(t.TempDir(), "restored")
	restoreRes, err := backup.Restore(ctx, repo, app, backup.RestoreOptions{TargetDir: target})
	require.NoError(err, "backup.Restore")
	assert.Equal(int64(2), restoreRes.AttachmentBlobs)

	restoredA, err := os.ReadFile(filepath.Join(target, "attachments",
		hashA[:2], hashA))
	require.NoError(err, "read restored blob A")
	assert.Equal(contentA, restoredA)
	// The packer canonicalized hashB's recorded path in the same transaction
	// that indexed it, so restore materializes it at the canonical location.
	restoredB, err := os.ReadFile(filepath.Join(target, "attachments",
		hashB[:2], hashB))
	require.NoError(err, "read restored blob B")
	assert.Equal(contentB, restoredB)

	_, err = os.Stat(filepath.Join(target, "attachments", "synctech-sms"))
	assert.ErrorIs(err, fs.ErrNotExist, "no legacy path should be restored")
}
