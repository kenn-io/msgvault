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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// vaultFixture is a small SQLite-backed archive: a real store with one
// message plus helpers to record attachment rows with matching loose blob
// files under a temp attachments dir. SQLite is intrinsic here — the kit
// backup engine snapshots a SQLite database file.
type vaultFixture struct {
	t       testing.TB
	store   *store.Store
	dbPath  string
	dataDir string
	attDir  string
	maint   *packstore.Maintainer
	blobs   *attachmentstore.Store
	msgID   int64
	seq     int
}

func newVaultFixture(tb testing.TB) *vaultFixture {
	tb.Helper()
	require := require.New(tb)
	dataDir := tb.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "open store")
	tb.Cleanup(func() { _ = st.Close() })
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
	attDir := filepath.Join(dataDir, "attachments")
	layout, err := packstore.NewLayout(attDir, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err, "create pack layout")
	maint, err := packstore.NewMaintainer(store.NewPackCatalog(st), layout, packstore.MaintainerOptions{})
	require.NoError(err, "create pack maintainer")
	tb.Cleanup(func() { _ = maint.Close() })
	return &vaultFixture{
		t:       tb,
		store:   st,
		dbPath:  dbPath,
		dataDir: dataDir,
		attDir:  attDir,
		maint:   maint,
		blobs:   attachmentstore.Wrap(maint.Store()),
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
	return backupapp.NewContentSource(f.blobs, f.attDir)
}

// blockingSecondLookupIndex freezes a packstore.Open after it has observed
// the target hash as loose on both index lookups. This lets a test run the
// production packer before Open returns that now-stale result to its caller.
type blockingSecondLookupIndex struct {
	inner   packstore.Resolver
	hash    packstore.Hash
	reached chan struct{}
	release chan struct{}
	mu      sync.Mutex
	lookups int
}

func (i *blockingSecondLookupIndex) Resolve(ctx context.Context, hash packstore.Hash) (packstore.Location, error) {
	loc, err := i.inner.Resolve(ctx, hash)
	if err != nil {
		return loc, fmt.Errorf("resolve blocked hash: %w", err)
	}
	if hash != i.hash {
		return loc, nil
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
func (f *vaultFixture) pack() packstore.PackStats {
	f.t.Helper()
	stats, err := f.maint.Pack(context.Background(), packstore.PackOptions{})
	require.NoError(f.t, err, "packstore.Pack")
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

func TestBackupCapturesLooseBlobAboveMaintenanceLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	f := newVaultFixture(t)
	content := randomBytes(t, 1024)
	h := f.addBlob(content, canonicalPath(hashOf(content)))

	limits := packstore.DefaultLimits()
	limits.BlobBytes = 64
	layout, err := packstore.NewLayout(f.attDir, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)
	physical, err := packstore.NewStore(store.NewPackCatalog(f.store), layout, packstore.StoreOptions{Limits: limits})
	require.NoError(err)
	blobs := attachmentstore.Wrap(physical)
	t.Cleanup(func() { require.NoError(blobs.Close()) })

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	app := backupapp.New("test")
	manifest, err := backup.Create(ctx, repo, app, backup.CreateOptions{
		DBPath: f.dbPath, ContentDir: f.attDir, DataDir: f.dataDir,
		ContentSource: backupapp.NewContentSource(blobs, f.attDir),
	})
	require.NoError(err)
	assert.Equal(int64(1), manifest.Attachments.Blobs)
	assert.Equal(int64(len(content)), manifest.Attachments.BlobBytes)

	verified, err := backup.Verify(ctx, repo, app, backup.VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(verified.Problems)

	stream, size, err := blobs.OpenStream(ctx, h)
	require.NoError(err)
	assert.Equal(int64(len(content)), size)
	assert.Equal(content, readAllAndClose(t, stream))
	_, _, err = blobs.ReadBounded(h, int64(len(content)))
	var limitErr *packstore.LimitError
	require.ErrorAs(err, &limitErr, "bounded reads must retain the configured maintenance ceiling")
	assert.Equal(packstore.LimitBlobRawBytes, limitErr.Dimension)
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
	parsedHash, err := packstore.ParseHash(h)
	require.NoError(err)
	index := &blockingSecondLookupIndex{
		inner:   store.NewPackCatalog(f.store),
		hash:    parsedHash,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	blobs, err := attachmentstore.New(index, f.attDir)
	require.NoError(err)
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

// TestBackupPackedRestoreLifecycle proves capture and restore preserve the
// packed foundation end to end: a fully packed vault round-trips through
// backup, restores without eligible loose files, remains readable, can be
// backed up again, and survives unpack/repack with identical hashes.
func TestBackupPackedRestoreLifecycle(t *testing.T) {
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
	sourceRecords, err := f.store.ListPackRecords()
	require.NoError(err)
	require.NotEmpty(sourceRecords)
	sourcePackIDs := make(map[string]struct{}, len(sourceRecords))
	for _, record := range sourceRecords {
		sourcePackIDs[record.PackID] = struct{}{}
	}

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
	restoreRes, err := backup.Restore(ctx, repo, app, backup.RestoreOptions{
		TargetDir: target, PackedContent: backupapp.NewPackedRestoreTarget(packstore.DefaultLimits()),
	})
	require.NoError(err, "backup.Restore")
	assert.Equal(int64(2), restoreRes.AttachmentBlobs)
	assert.Equal(int64(2), restoreRes.PackedAttachmentBlobs)
	assert.Zero(restoreRes.LooseAttachmentBlobs)
	assert.Positive(restoreRes.AttachmentPacks)
	assert.Empty(restoreRes.PackFallbacks)

	restoredStore, err := store.OpenForTest(restoreRes.DBPath)
	require.NoError(err, "open restored store")
	t.Cleanup(func() { _ = restoredStore.Close() })
	restoredAttDir := filepath.Join(target, "attachments")
	blobs, err := attachmentstore.New(store.NewPackCatalog(restoredStore), restoredAttDir)
	require.NoError(err)
	for h, want := range map[string][]byte{hashA: contentA, hashB: contentB} {
		r, size, err := blobs.Open(h)
		require.NoErrorf(err, "attachmentstore.Open(%s) after packed restore", h)
		assert.Equalf(int64(len(want)), size, "blob %s size", h)
		assert.Equalf(want, readAllAndClose(t, r), "blob %s reads byte-identical", h)
		_, err = os.Stat(filepath.Join(restoredAttDir, h[:2], h))
		require.ErrorIs(err, fs.ErrNotExist, "eligible restored blob must not be materialized loose")
	}
	packRecords, err := restoredStore.ListPackRecords()
	require.NoError(err)
	assert.Len(packRecords, restoreRes.AttachmentPacks)
	for _, record := range packRecords {
		_, staleSourceID := sourcePackIDs[record.PackID]
		assert.False(staleSourceID, "restore must replace source-vault pack metadata with repository pack IDs")
	}
	indexed, err := restoredStore.ListIndexedBlobHashes()
	require.NoError(err)
	assert.Equal(map[string]struct{}{hashA: {}, hashB: {}}, indexed)

	secondRepo, err := backup.Init(filepath.Join(t.TempDir(), "restored-repo"))
	require.NoError(err)
	_, err = backup.Create(ctx, secondRepo, app, backup.CreateOptions{
		DBPath: restoreRes.DBPath, ContentDir: restoredAttDir, DataDir: target,
		ContentSource: backupapp.NewContentSource(blobs, restoredAttDir),
	})
	require.NoError(err, "back up packed restored vault")
	require.NoError(blobs.Close())
	secondVerify, err := backup.Verify(ctx, secondRepo, app, backup.VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(secondVerify.Problems)

	layout, err := packstore.NewLayout(restoredAttDir, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	maint, err := packstore.NewMaintainer(store.NewPackCatalog(restoredStore), layout, packstore.MaintainerOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = maint.Close() })
	unpacked, err := maint.Unpack(ctx)
	require.NoError(err)
	assert.Equal(2, unpacked.BlobsRestored)
	for h, want := range map[string][]byte{hashA: contentA, hashB: contentB} {
		got, err := os.ReadFile(filepath.Join(restoredAttDir, h[:2], h))
		require.NoError(err)
		assert.Equal(want, got)
	}
	repacked, err := maint.Pack(ctx, packstore.PackOptions{})
	require.NoError(err)
	assert.Equal(2, repacked.BlobsPacked)
	cycleBlobs := attachmentstore.Wrap(maint.Store())
	for h, want := range map[string][]byte{hashA: contentA, hashB: contentB} {
		r, size, err := cycleBlobs.Open(h)
		require.NoError(err)
		assert.Equal(int64(len(want)), size)
		assert.Equal(want, readAllAndClose(t, r))
	}

	looseTarget := filepath.Join(t.TempDir(), "restored-loose")
	looseRes, err := backup.Restore(ctx, repo, app, backup.RestoreOptions{TargetDir: looseTarget})
	require.NoError(err)
	assert.Zero(looseRes.PackedAttachmentBlobs)
	assert.Equal(int64(2), looseRes.LooseAttachmentBlobs)
	_, err = os.Stat(filepath.Join(looseTarget, "attachments", "packs"))
	require.ErrorIs(err, fs.ErrNotExist)
	looseStore, err := store.OpenForTest(looseRes.DBPath)
	require.NoError(err)
	defer func() { require.NoError(looseStore.Close()) }()
	require.NoError(looseStore.ClearAttachmentPackMetadata(), "mirror --loose-attachments CLI cleanup")
	looseBlobs, err := attachmentstore.New(store.NewPackCatalog(looseStore), filepath.Join(looseTarget, "attachments"))
	require.NoError(err)
	defer func() { require.NoError(looseBlobs.Close()) }()
	for h, want := range map[string][]byte{hashA: contentA, hashB: contentB} {
		r, _, err := looseBlobs.Open(h)
		require.NoError(err)
		assert.Equal(want, readAllAndClose(t, r))
	}
}

func TestBackupPackedRestoreOverwriteReplacesPriorPackAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	snapshot := newVaultFixture(t)
	snapshotContent := []byte("attachment retained by the restored snapshot")
	snapshotHash := snapshot.addBlob(snapshotContent, canonicalPath(hashOf(snapshotContent)))
	require.Equal(1, snapshot.pack().BlobsPacked)
	snapshotRepo, err := backup.Init(filepath.Join(t.TempDir(), "snapshot-repo"))
	require.NoError(err)
	app := backupapp.New("test")
	_, err = backup.Create(ctx, snapshotRepo, app, backup.CreateOptions{
		DBPath: snapshot.dbPath, ContentDir: snapshot.attDir, DataDir: snapshot.dataDir,
		ContentSource: snapshot.contentSource(),
	})
	require.NoError(err)

	newer := newVaultFixture(t)
	newerContent := []byte("attachment that exists only in the overwritten target")
	newerHash := newer.addBlob(newerContent, canonicalPath(hashOf(newerContent)))
	require.Equal(1, newer.pack().BlobsPacked)
	newerRepo, err := backup.Init(filepath.Join(t.TempDir(), "newer-repo"))
	require.NoError(err)
	_, err = backup.Create(ctx, newerRepo, app, backup.CreateOptions{
		DBPath: newer.dbPath, ContentDir: newer.attDir, DataDir: newer.dataDir,
		ContentSource: newer.contentSource(),
	})
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "overwrite-target")
	newerRestore, err := backup.Restore(ctx, newerRepo, app, backup.RestoreOptions{
		TargetDir: target, PackedContent: backupapp.NewPackedRestoreTarget(packstore.DefaultLimits()),
	})
	require.NoError(err)
	newerStore, err := store.OpenForTest(newerRestore.DBPath)
	require.NoError(err)
	newerRecords, err := newerStore.ListPackRecords()
	require.NoError(err)
	require.Len(newerRecords, 1)
	require.NoError(newerStore.Close())
	targetLayout, err := packstore.NewLayout(filepath.Join(target, "attachments"), packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)
	oldPackPath := targetLayout.PackPath(newerRecords[0].PackID)
	require.FileExists(oldPackPath)

	restored, err := backup.Restore(ctx, snapshotRepo, app, backup.RestoreOptions{
		TargetDir: target, Overwrite: true,
		PackedContent: backupapp.NewPackedRestoreTarget(packstore.DefaultLimits()),
	})
	require.NoError(err)
	restoredStore, err := store.OpenForTest(restored.DBPath)
	require.NoError(err)
	defer func() { require.NoError(restoredStore.Close()) }()
	restoredRecords, err := restoredStore.ListPackRecords()
	require.NoError(err)
	require.Len(restoredRecords, 1)
	assert.NotEqual(newerRecords[0].PackID, restoredRecords[0].PackID)
	indexed, err := restoredStore.ListIndexedBlobHashes()
	require.NoError(err)
	assert.Equal(map[string]struct{}{snapshotHash: {}}, indexed,
		"overwrite must grant authority only to snapshot-referenced content")
	assert.FileExists(oldPackPath,
		"overwrite merge preserves old physical files until normal maintenance reconciles them")

	blobs, err := attachmentstore.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "attachments"))
	require.NoError(err)
	defer func() { require.NoError(blobs.Close()) }()
	reader, _, err := blobs.Open(snapshotHash)
	require.NoError(err)
	assert.Equal(snapshotContent, readAllAndClose(t, reader))
	_, _, err = blobs.Open(newerHash)
	require.ErrorIs(err, fs.ErrNotExist,
		"a preserved old pack file must not grant authority to non-snapshot content")
}

func TestBackupPackedRestoreMixedConfiguredLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newVaultFixture(t)
	ctx := context.Background()
	small := []byte("small packed blob")
	large := randomBytes(t, 1024)
	smallHash := f.addBlob(small, canonicalPath(hashOf(small)))
	largeHash := f.addBlob(large, canonicalPath(hashOf(large)))
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	app := backupapp.New("test")
	_, err = backup.Create(ctx, repo, app, backup.CreateOptions{
		DBPath: f.dbPath, ContentDir: f.attDir, DataDir: f.dataDir,
		ContentSource: f.contentSource(),
	})
	require.NoError(err)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = 64
	target := filepath.Join(t.TempDir(), "mixed")
	res, err := backup.Restore(ctx, repo, app, backup.RestoreOptions{
		TargetDir: target, PackedContent: backupapp.NewPackedRestoreTarget(limits),
	})
	require.NoError(err)
	assert.Equal(int64(2), res.AttachmentBlobs)
	assert.Equal(int64(1), res.PackedAttachmentBlobs)
	assert.Equal(int64(1), res.LooseAttachmentBlobs)
	var fallbackHashes []string
	for _, fallback := range res.PackFallbacks {
		if fallback.Hash != "" {
			fallbackHashes = append(fallbackHashes, fallback.Hash.String())
		}
	}
	assert.Equal([]string{largeHash}, fallbackHashes)
	assert.NoFileExists(filepath.Join(target, "attachments", smallHash[:2], smallHash))
	assert.FileExists(filepath.Join(target, "attachments", largeHash[:2], largeHash))

	restoredStore, err := store.OpenForTest(res.DBPath)
	require.NoError(err)
	defer func() { require.NoError(restoredStore.Close()) }()
	blobs, err := attachmentstore.New(store.NewPackCatalog(restoredStore), filepath.Join(target, "attachments"))
	require.NoError(err)
	defer func() { require.NoError(blobs.Close()) }()
	for h, want := range map[string][]byte{smallHash: small, largeHash: large} {
		r, _, err := blobs.Open(h)
		require.NoError(err)
		assert.Equal(want, readAllAndClose(t, r))
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
	oldPath := filepath.Join(f.attDir, "packs", oldPackID[:2], oldPackID+packstore.PackExt)

	backupBlobs, err := attachmentstore.New(store.NewPackCatalog(f.store), f.attDir)
	require.NoError(err)
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
	repackStats, repackErr := f.maint.Repack(context.Background(), packstore.RepackOptions{})
	require.NoError(repackErr)
	assert.Equal(1, repackStats.PacksRemoved)
	assert.NoFileExists(oldPath)
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
	verify, err := backup.Verify(context.Background(), repo, backupapp.New("test"), backup.VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(verify.Problems)
	r, _, err := f.blobs.Open(liveHash)
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

	blobs, err := attachmentstore.New(store.NewPackCatalog(f.store), f.attDir)
	require.NoError(err)
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
