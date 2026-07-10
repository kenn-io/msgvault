package backupapp_test

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/repacker"
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

// blockingSecondLookupIndex freezes a blobstore.Open after it has observed
// the target hash as loose on both index lookups. This lets a test run the
// production packer before Open returns that now-stale result to its caller.
type blockingSecondLookupIndex struct {
	inner   blobstore.PackIndex
	hash    string
	reached chan struct{}
	release chan struct{}
	mu      sync.Mutex
	lookups int
}

func (i *blockingSecondLookupIndex) ResolveAttachmentBlob(hash string) (store.AttachmentBlobLocation, error) {
	loc, err := i.inner.ResolveAttachmentBlob(hash)
	if hash != i.hash || err != nil {
		return loc, err
	}
	i.mu.Lock()
	i.lookups++
	lookup := i.lookups
	i.mu.Unlock()
	if lookup == 2 {
		close(i.reached)
		<-i.release
	}
	return loc, nil
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

func TestContentSourceRetriesPackedLookupWhenLegacyOpenLosesRace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)

	content := []byte("legacy payload packed during backup open")
	h := hashOf(content)
	rel := "synctech-sms/" + h[:2] + "/" + h
	f.addBlob(content, rel)
	index := &blockingSecondLookupIndex{
		inner: f.store, hash: h, reached: make(chan struct{}), release: make(chan struct{}),
	}
	blobs := blobstore.New(index, f.attDir)
	t.Cleanup(func() { require.NoError(blobs.Close()) })
	src := backupapp.NewContentSource(blobs, f.attDir)

	type openResult struct {
		rc  io.ReadCloser
		err error
	}
	opened := make(chan openResult, 1)
	go func() {
		rc, err := src.Open(context.Background(), backup.ContentRef{
			Hash: h, Size: int64(len(content)), StoragePath: rel,
		})
		opened <- openResult{rc: rc, err: err}
	}()

	select {
	case <-index.reached:
	case <-time.After(10 * time.Second):
		require.FailNow("blob store did not reach its second loose index lookup")
	}
	stats := f.pack()
	require.Equal(1, stats.BlobsPacked)
	assert.Empty(f.looseFiles(), "packer must remove the legacy source")
	close(index.release)

	var result openResult
	select {
	case result = <-opened:
	case <-time.After(10 * time.Second):
		require.FailNow("content source did not finish after packer committed")
	}
	require.NoError(result.err)
	assert.Equal(content, readAllAndClose(t, result.rc))
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
	require.ErrorIs(err, fs.ErrNotExist, "no legacy path should be restored")

	// A restored vault has loose files but NO production pack files, so the
	// pack metadata carried in the restored DB is dangling. Reads through the
	// production blob store must fail while it remains (index hit -> missing
	// pack -> single index retry -> fail; no loose fallback by design) — this
	// is exactly why `backup restore` clears the metadata.
	restoredStore, err := store.OpenForTest(restoreRes.DBPath)
	require.NoError(err, "open restored store")
	t.Cleanup(func() { _ = restoredStore.Close() })
	restoredAttDir := filepath.Join(target, "attachments")

	stale := blobstore.New(restoredStore, restoredAttDir)
	_, _, err = stale.Open(hashA)
	require.Error(err, "packed-blob read must fail while stale pack metadata remains")
	require.ErrorIs(err, fs.ErrNotExist, "failure is the missing pack file, not the loose copy")
	require.NoError(stale.Close(), "close stale blob store")

	// Mirror the CLI restore flow (cmd/msgvault/cmd/backup.go): InitSchema is
	// idempotent and guarantees the pack tables exist even for snapshots
	// predating them, then the metadata is cleared.
	require.NoError(restoredStore.InitSchema(), "init restored schema")
	require.NoError(restoredStore.ClearAttachmentPackMetadata(), "clear restored pack metadata")

	blobs := blobstore.New(restoredStore, restoredAttDir)
	t.Cleanup(func() { _ = blobs.Close() })
	for h, want := range map[string][]byte{hashA: contentA, hashB: contentB} {
		r, size, err := blobs.Open(h)
		require.NoErrorf(err, "blobstore.Open(%s) after clearing pack metadata", h)
		assert.Equalf(int64(len(want)), size, "blob %s size", h)
		assert.Equalf(want, readAllAndClose(t, r), "blob %s reads byte-identical", h)
	}
}

type blockingOpenedSource struct {
	inner   backup.ContentSource
	opened  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingOpenedSource) Open(ctx context.Context, ref backup.ContentRef) (io.ReadCloser, error) {
	r, err := s.inner.Open(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("open real backup content source: %w", err)
	}
	s.once.Do(func() { close(s.opened) })
	select {
	case <-ctx.Done():
		_ = r.Close()
		return nil, ctx.Err()
	case <-s.release:
		return r, nil
	}
}

type blockingBeforeOpenSource struct {
	inner   backup.ContentSource
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingBeforeOpenSource) Open(ctx context.Context, ref backup.ContentRef) (io.ReadCloser, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		r, err := s.inner.Open(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("open released backup content source: %w", err)
		}
		return r, nil
	}
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	_, err := crand.Read(data)
	require.NoError(t, err)
	return data
}

func makeSparsePackedVault(t *testing.T, f *vaultFixture) (string, []byte, string) {
	t.Helper()
	live := []byte("backup captures byte-identical live content while repack overlaps")
	liveHash := f.addBlob(live, canonicalPath(hashOf(live)))
	deadA := randomBytes(t, (8<<20)+(256<<10))
	deadAHash := f.addBlob(deadA, canonicalPath(hashOf(deadA)))
	deadB := []byte("second dead entry makes the live fraction strictly below half")
	deadBHash := f.addBlob(deadB, canonicalPath(hashOf(deadB)))
	stats := f.pack()
	require.Equal(t, 3, stats.BlobsPacked)

	entry, err := f.store.GetAttachmentPackEntry(liveHash)
	require.NoError(t, err)
	require.NotNil(t, entry)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		DELETE FROM attachments WHERE content_hash IN (?, ?)`), deadAHash, deadBHash)
	require.NoError(t, err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339), entry.PackID)
	require.NoError(t, err)
	return liveHash, live, entry.PackID
}

func TestBackupCaptureOverlapsRepack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	liveHash, live, oldPackID := makeSparsePackedVault(t, f)
	oldPath := filepath.Join(f.attDir, "packs", oldPackID[:2], oldPackID+blobstore.PackExt)

	backupBlobs := blobstore.New(f.store, f.attDir)
	realSource := backupapp.NewContentSource(backupBlobs, f.attDir)
	blocked := &blockingOpenedSource{
		inner: realSource, opened: make(chan struct{}), release: make(chan struct{}),
	}
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	type createResult struct {
		manifest *backup.Manifest
		err      error
	}
	created := make(chan createResult, 1)
	go func() {
		manifest, createErr := backup.Create(context.Background(), repo, backupapp.New("test"), backup.CreateOptions{
			DBPath: f.dbPath, ContentDir: f.attDir, DataDir: f.dataDir,
			ContentSource: blocked, Jobs: 1,
		})
		created <- createResult{manifest: manifest, err: createErr}
	}()

	select {
	case <-blocked.opened:
	case <-time.After(10 * time.Second):
		require.FailNow("backup did not open the old packed blob")
	}
	daemonBlobs := blobstore.New(f.store, f.attDir)
	repackStats, repackErr := repacker.Run(context.Background(), f.store, daemonBlobs, f.attDir, repacker.Options{})
	if runtime.GOOS == "windows" {
		require.Error(repackErr, "backup-held independent reader must make Windows deletion retryable")
		has, hasErr := f.store.HasPackRecord(oldPackID)
		require.NoError(hasErr)
		assert.True(has)
		assert.FileExists(oldPath)
	} else {
		require.NoError(repackErr)
		assert.Equal(1, repackStats.PacksRemoved)
		assert.NoFileExists(oldPath)
	}
	close(blocked.release)
	var result createResult
	select {
	case result = <-created:
	case <-time.After(10 * time.Second):
		require.FailNow("backup did not finish after releasing its content source")
	}
	require.NoError(result.err)
	require.NotNil(result.manifest)
	assert.Equal(int64(1), result.manifest.Attachments.Blobs)
	require.NoError(backupBlobs.Close())
	if runtime.GOOS == "windows" {
		retryStats, retryErr := repacker.Run(context.Background(), f.store, daemonBlobs, f.attDir, repacker.Options{})
		require.NoError(retryErr)
		assert.Equal(1, retryStats.PacksRemoved)
		assert.NoFileExists(oldPath)
	}
	require.NoError(daemonBlobs.Close())

	verify, err := backup.Verify(context.Background(), repo, backupapp.New("test"), backup.VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(verify.Problems)
	current := blobstore.New(f.store, f.attDir)
	defer func() { require.NoError(current.Close()) }()
	r, _, err := current.Open(liveHash)
	require.NoError(err)
	assert.Equal(live, readAllAndClose(t, r))
}

func TestBackupCaptureFailsLoudlyAfterLogicalDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	content := []byte("snapshot-only reference must not be omitted silently")
	hash := f.addBlob(content, canonicalPath(hashOf(content)))
	f.pack()

	blobs := blobstore.New(f.store, f.attDir)
	defer func() { require.NoError(blobs.Close()) }()
	blocked := &blockingBeforeOpenSource{
		inner:   backupapp.NewContentSource(blobs, f.attDir),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	created := make(chan error, 1)
	go func() {
		_, createErr := backup.Create(context.Background(), repo, backupapp.New("test"), backup.CreateOptions{
			DBPath: f.dbPath, ContentDir: f.attDir, DataDir: f.dataDir,
			ContentSource: blocked, Jobs: 1,
		})
		created <- createErr
	}()
	select {
	case <-blocked.started:
	case <-time.After(10 * time.Second):
		require.FailNow("backup did not reach attachment capture")
	}
	_, err = f.store.DB().Exec(f.store.Rebind(`DELETE FROM attachments WHERE content_hash = ?`), hash)
	require.NoError(err)
	close(blocked.release)
	var createErr error
	select {
	case createErr = <-created:
	case <-time.After(10 * time.Second):
		require.FailNow("backup did not fail after releasing its deleted content reference")
	}
	require.Error(createErr)
	assert.Contains(createErr.Error(), hash)
}
