package packer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type maintenanceFixture struct {
	t     *testing.T
	store *store.Store
	dir   string
	msgID int64
	seq   int
}

type cancelAfterErrContext struct {
	context.Context

	calls       int
	cancelAfter int
}

type canonicalizationFailurePoint int

const (
	failLegacyCanonicalization canonicalizationFailurePoint = iota
	failSweepCanonicalization
)

func installCanonicalizationFailure(t *testing.T, st *store.Store, point canonicalizationFailurePoint) string {
	t.Helper()

	var triggerName, functionName, failureMessage string
	switch point {
	case failLegacyCanonicalization:
		triggerName = "block_blob_canonicalization"
		functionName = "block_blob_canonicalization_fn"
		failureMessage = "injected canonicalization failure"
	case failSweepCanonicalization:
		triggerName = "block_sweep_canonicalization"
		functionName = "block_sweep_canonicalization_fn"
		failureMessage = "injected sweep canonicalization failure"
	default:
		require.FailNow(t, "unknown canonicalization failure point", "point=%d", point)
	}

	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION '%s';
			END;
			$$ LANGUAGE plpgsql`, functionName, failureMessage))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, cleanupErr := st.DB().Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
			require.NoError(t, cleanupErr)
		})

		_, err = st.DB().Exec(fmt.Sprintf(`
			CREATE TRIGGER %s
			BEFORE UPDATE OF storage_path ON attachments
			FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, cleanupErr := st.DB().Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON attachments", triggerName))
			require.NoError(t, cleanupErr)
		})
		return failureMessage
	}

	_, err := st.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF storage_path ON attachments
		BEGIN SELECT RAISE(ABORT, '%s'); END`, triggerName, failureMessage))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := st.DB().Exec("DROP TRIGGER IF EXISTS " + triggerName)
		require.NoError(t, cleanupErr)
	})
	return failureMessage
}

const packingRecordFailure = "injected target pack recording failure"

func installPackingRecordFailureForHash(t *testing.T, st *store.Store, hash string) {
	t.Helper()

	const (
		triggerName  = "block_target_pack_recording"
		functionName = "block_target_pack_recording_fn"
	)
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger AS $$
			BEGIN
				IF LOWER(OLD.content_hash) = '%s' THEN
					RAISE EXCEPTION '%s';
				END IF;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql`, functionName, hash, packingRecordFailure))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, cleanupErr := st.DB().Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
			require.NoError(t, cleanupErr)
		})

		_, err = st.DB().Exec(fmt.Sprintf(`
			CREATE TRIGGER %s
			BEFORE UPDATE OF storage_path ON attachments
			FOR EACH ROW EXECUTE FUNCTION %s()`, triggerName, functionName))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, cleanupErr := st.DB().Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON attachments", triggerName))
			require.NoError(t, cleanupErr)
		})
		return
	}

	_, err := st.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF storage_path ON attachments
		WHEN LOWER(OLD.content_hash) = '%s'
		BEGIN SELECT RAISE(ABORT, '%s'); END`, triggerName, hash, packingRecordFailure))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := st.DB().Exec("DROP TRIGGER IF EXISTS " + triggerName)
		require.NoError(t, cleanupErr)
	})
}

func (c *cancelAfterErrContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}

func newMaintenanceFixture(t *testing.T) *maintenanceFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(src.ID, "maintenance-thread", "Maintenance Thread")
	require.NoError(t, err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "maintenance-message",
		MessageType:     "email",
	})
	require.NoError(t, err)
	return &maintenanceFixture{t: t, store: st, dir: t.TempDir(), msgID: msgID}
}

func maintenanceHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func maintenanceCanonical(hash string) string { return hash[:2] + "/" + hash }

func mustNormalizedBlobHash(t *testing.T, hash string) normalizedBlobHash {
	t.Helper()
	normalized, err := normalizeBlobHash(hash)
	require.NoError(t, err)
	return normalized
}

func (f *maintenanceFixture) addRow(hash, relPath string, size int) {
	f.t.Helper()
	f.seq++
	err := f.store.UpsertAttachment(f.msgID, fmt.Sprintf("maintenance-%d.bin", f.seq),
		"application/octet-stream", relPath, hash, size)
	require.NoError(f.t, err)
}

func (f *maintenanceFixture) write(relPath string, content []byte) string {
	f.t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(relPath))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(f.t, os.WriteFile(path, content, 0o600))
	return path
}

func (f *maintenanceFixture) addBlob(content []byte, relPath func(string) string) (string, string) {
	f.t.Helper()
	hash := maintenanceHash(content)
	rel := relPath(hash)
	f.addRow(hash, rel, len(content))
	return hash, f.write(rel, content)
}

func (f *maintenanceFixture) read(hash string) []byte {
	f.t.Helper()
	bs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(f.t, bs.Close()) }()
	r, _, err := bs.Open(hash)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, r.Close()) }()
	data, err := io.ReadAll(r)
	require.NoError(f.t, err)
	return data
}

func (f *maintenanceFixture) storagePath(hash string) string {
	f.t.Helper()
	var path string
	err := f.store.DB().QueryRow(f.store.Rebind(`
		SELECT storage_path FROM attachments WHERE content_hash = ? LIMIT 1`), hash).Scan(&path)
	require.NoError(f.t, err)
	return path
}

func setMaintenanceTestLimits(t *testing.T, packEntries int) {
	t.Helper()
	oldBlobBytes := maintenanceBlobBytes
	oldPackEntries := maintenancePackEntries
	maintenanceBlobBytes = 1024
	maintenancePackEntries = packEntries
	t.Cleanup(func() {
		maintenanceBlobBytes = oldBlobBytes
		maintenancePackEntries = oldPackEntries
	})
}

func buildMaintenanceOrphan(t *testing.T, dir string, blobs ...[]byte) (string, string) {
	t.Helper()
	packsDir := filepath.Join(dir, "packs")
	require.NoError(t, os.MkdirAll(packsDir, 0o700))
	w, err := pack.NewWriter(packsDir, pack.WriterOptions{})
	require.NoError(t, err)
	for _, blob := range blobs {
		_, err := w.Append(blob)
		require.NoError(t, err)
	}
	id := w.ID()
	path := filepath.Join(packsDir, id[:2], id+blobstore.PackExt)
	_, err = w.Seal(path)
	require.NoError(t, err)
	return id, path
}

func captureMaintenanceLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestRunAcceptsLooseBlobAtMaintenanceLimit(t *testing.T) {
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1024)
	hash, _ := f.addBlob(content, maintenanceCanonical)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Zero(stats.BlobsDeferredOversized)
	assert.Equal(content, f.read(hash))
}

func TestRunDoesNotCountPackWhenSealFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("loose blob retained after seal failure")
	_, loose := f.addBlob(content, maintenanceCanonical)
	packsDir := filepath.Join(f.dir, "packs")
	require.NoError(os.MkdirAll(packsDir, 0o700))
	for _, first := range "01234567" {
		for _, second := range "0123456789abcdefghjkmnpqrstvwxyz" {
			require.NoError(os.WriteFile(filepath.Join(packsDir, string([]rune{first, second})),
				[]byte("blocks pack shard directory"), 0o600))
		}
	}

	stats, err := Run(context.Background(), f.store, f.dir, Options{MaxBytes: 1})
	require.Error(err)
	assert.Zero(stats.PacksSealed)
	assert.Zero(stats.BlobsPacked)
	assert.Zero(stats.BytesPacked)
	assert.False(stats.BudgetExhausted)
	assert.FileExists(loose)
}

func TestRunDoesNotCountPackWhenDatabaseRecordFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("loose blob retained after database failure")
	hash, loose := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	installPackingRecordFailureForHash(t, f.store, hash)

	stats, err := Run(context.Background(), f.store, f.dir, Options{MaxBytes: 1})
	require.ErrorContains(err, packingRecordFailure)
	assert.Zero(stats.PacksSealed)
	assert.Zero(stats.BlobsPacked)
	assert.Zero(stats.BytesPacked)
	assert.False(stats.BudgetExhausted)
	assert.FileExists(loose)
	entry, lookupErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(lookupErr)
	assert.Nil(entry)
}

func TestRunCountsOnlyCommittedPackWhenLaterDatabaseRecordFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	firstContent := []byte("first committed loose blob")
	firstHash, firstLoose := f.addBlob(firstContent, func(hash string) string { return "legacy/" + hash })
	secondContent := []byte("second uncommitted loose blob")
	secondHash, secondLoose := f.addBlob(secondContent, func(hash string) string { return "legacy/" + hash })
	installPackingRecordFailureForHash(t, f.store, secondHash)

	stats, err := Run(context.Background(), f.store, f.dir, Options{TargetSize: 1})
	require.ErrorContains(err, packingRecordFailure)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(int64(len(firstContent)), stats.BytesPacked)
	assert.False(stats.BudgetExhausted)
	assert.NoFileExists(firstLoose)
	assert.FileExists(secondLoose)
	firstEntry, lookupErr := f.store.GetAttachmentPackEntry(firstHash)
	require.NoError(lookupErr)
	assert.NotNil(firstEntry)
	secondEntry, lookupErr := f.store.GetAttachmentPackEntry(secondHash)
	require.NoError(lookupErr)
	assert.Nil(secondEntry)
}

func TestRunDefersOversizedCanonicalBlobOncePerHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, path := f.addBlob(content, maintenanceCanonical)
	f.addRow(hash, maintenanceCanonical(hash), len(content))
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(path)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.Nil(entry)
	assert.Equal(content, f.read(hash))
	assert.Contains(logs.String(), "hash="+hash)
	assert.Contains(logs.String(), "raw_bytes=1025")
	assert.Contains(logs.String(), "max_raw_bytes=1024")
}

func TestRunDefersCanonicalOversizedBlobWithoutOpeningIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	want := make([]byte, 1025)
	want[0] = 1
	hash := maintenanceHash(want)
	path := f.write(maintenanceCanonical(hash), make([]byte, len(want)))
	f.addRow(hash, maintenanceCanonical(hash), len(want))
	oldOpen := openLooseFile
	var opens int
	openLooseFile = func(candidate string) (*os.File, error) {
		if candidate == path {
			opens++
			return nil, errors.New("oversized canonical file must not be opened")
		}
		return oldOpen(candidate)
	}
	t.Cleanup(func() { openLooseFile = oldOpen })

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(opens)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.BlobsCorrupt)
	assert.FileExists(path)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
}

func TestRunMigratesOversizedNoncanonicalBlobWithoutPacking(t *testing.T) {
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash)))

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.NoFileExists(legacy)
	assert.FileExists(canonical)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	assert.Equal(content, f.read(hash))
}

func TestRunRetainsOversizedLegacySourceUntilDatabaseCanonicalizes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	failureMessage := installCanonicalizationFailure(t, f.store, failLegacyCanonicalization)

	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.ErrorContains(err, failureMessage)
	assert.FileExists(legacy, "legacy source remains authoritative after the DB failure")
	assert.Equal("legacy/"+hash, f.storagePath(hash))
	assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash))),
		"a verified canonical copy may remain as harmless crash-recovery data")
}

func TestRunRetriesOversizedLegacyDeletionAfterCanonicalCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	oldRemove := removeLooseFile
	removeLooseFile = func(path string) error {
		if path == legacy {
			return errors.New("injected legacy removal failure")
		}
		return oldRemove(path)
	}
	t.Cleanup(func() { removeLooseFile = oldRemove })

	first, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, first.BlobsDeferredOversized)
	assert.FileExists(legacy)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	assert.Equal(content, f.read(hash))

	removeLooseFile = oldRemove
	second, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.NoFileExists(legacy)
	assert.Equal(1, second.LooseSwept)
	assert.Equal(content, f.read(hash))
}

func TestRunDefersWholeOrphanWhenAnyEntryIsOversized(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	small := make([]byte, 512)
	large := make([]byte, 1025)
	for _, content := range [][]byte{small, large} {
		hash := maintenanceHash(content)
		f.addRow(hash, maintenanceCanonical(hash), len(content))
	}
	id, path := buildMaintenanceOrphan(t, f.dir, small, large)
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.PacksAdopted)
	assert.FileExists(path)
	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.False(has)
	for _, content := range [][]byte{small, large} {
		entry, err := f.store.GetAttachmentPackEntry(maintenanceHash(content))
		require.NoError(err)
		assert.Nil(entry, "no entry from an oversized orphan may be adopted")
	}
	assert.Contains(logs.String(), "pack="+id)
	assert.Contains(logs.String(), "raw_bytes=1025")
	assert.Contains(logs.String(), "max_raw_bytes=1024")
	assert.Contains(logs.String(), "withheld_entries=2")
}

func TestRunAdoptsSmallReferencedCandidateDespiteDeadOversizedOrphanEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	small := []byte("small referenced orphan candidate")
	largeDead := make([]byte, 1025)
	smallHash := maintenanceHash(small)
	f.addRow(smallHash, maintenanceCanonical(smallHash), len(small))
	id, path := buildMaintenanceOrphan(t, f.dir, small, largeDead)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(stats.PacksDeferredOversized)
	assert.Equal(1, stats.PacksAdopted)
	assert.FileExists(path)
	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.True(has)
	entry, err := f.store.GetAttachmentPackEntry(smallHash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(id, entry.PackID)
	assert.Equal(small, f.read(smallHash))
}

func TestRunDefersWholeOrphanBeforeReadingCandidateWhenExistingCopyIsOversized(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	large := make([]byte, 1025)
	largeHash, _ := f.addBlob(large, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	existing, err := f.store.GetAttachmentPackEntry(largeHash)
	require.NoError(err)
	require.NotNil(existing)

	small := []byte("small candidate withheld by oversized redundancy check")
	smallHash := maintenanceHash(small)
	f.addRow(smallHash, maintenanceCanonical(smallHash), len(small))
	orphanID, orphanPath := buildMaintenanceOrphan(t, f.dir, small, large)
	r, err := blobstore.OpenMaintenancePack(orphanPath)
	require.NoError(err)
	var largeOffset int64
	for _, entry := range r.Entries() {
		if entry.ID.String() == largeHash {
			largeOffset = int64(entry.Offset)
		}
	}
	require.NoError(r.Close())
	require.Positive(largeOffset)
	packFile, err := os.OpenFile(orphanPath, os.O_RDWR, 0)
	require.NoError(err)
	var corrupt [1]byte
	_, err = packFile.ReadAt(corrupt[:], largeOffset)
	require.NoError(err)
	corrupt[0] ^= 0xff
	_, err = packFile.WriteAt(corrupt[:], largeOffset)
	require.NoError(err)
	require.NoError(packFile.Close())
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksQuarantined, "the damaged orphan candidate must not be read")
	has, err := f.store.HasPackRecord(orphanID)
	require.NoError(err)
	assert.False(has)
	smallEntry, err := f.store.GetAttachmentPackEntry(smallHash)
	require.NoError(err)
	assert.Nil(smallEntry, "all candidates are withheld together")
	largeEntry, err := f.store.GetAttachmentPackEntry(largeHash)
	require.NoError(err)
	require.NotNil(largeEntry)
	assert.Equal(existing.PackID, largeEntry.PackID)
	assert.Contains(logs.String(), "raw_bytes=1025")
	assert.Contains(logs.String(), "max_raw_bytes=1024")
	assert.Contains(logs.String(), "withheld_entries=2")
}

func TestRunPlansAllOrphanEntryLimitsBeforeExistingContentReads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	small := []byte("small existing packed blob")
	large := make([]byte, 1025)
	f.addBlob(small, maintenanceCanonical)
	f.addBlob(large, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	_, orphanPath := buildMaintenanceOrphan(t, f.dir, small, large)
	assert.FileExists(orphanPath)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	oldRead := readExistingBounded
	reads := 0
	readExistingBounded = func(blobs *blobstore.Store, hash string, limit int64) ([]byte, int64, error) {
		reads++
		return oldRead(blobs, hash, limit)
	}
	t.Cleanup(func() { readExistingBounded = oldRead })

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(reads, "a later oversized referenced entry must prevent every earlier content read")
}

func TestRunLogsExistingPackContainerLimitWithoutMislabelingRawBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("small blob in an oversized existing pack container")
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	existing, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(existing)
	existingPath := filepath.Join(f.dir, "packs", existing.PackID[:2], existing.PackID+blobstore.PackExt)
	actualPackBytes := int64(blobstore.MaxMaintenancePackBytes + 1)
	require.NoError(os.Truncate(existingPath, actualPackBytes))

	orphanID, orphanPath := buildMaintenanceOrphan(t, f.dir, content)
	r, err := blobstore.OpenMaintenancePack(orphanPath)
	require.NoError(err)
	entries := r.Entries()
	require.Len(entries, 1)
	orphanOffset := int64(entries[0].Offset)
	require.NoError(r.Close())
	packFile, err := os.OpenFile(orphanPath, os.O_RDWR, 0)
	require.NoError(err)
	var corrupt [1]byte
	_, err = packFile.ReadAt(corrupt[:], orphanOffset)
	require.NoError(err)
	corrupt[0] ^= 0xff
	_, err = packFile.WriteAt(corrupt[:], orphanOffset)
	require.NoError(err)
	require.NoError(packFile.Close())
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.PacksQuarantined, "pack metadata deferral must happen before the orphan blob read")
	has, err := f.store.HasPackRecord(orphanID)
	require.NoError(err)
	assert.False(has)
	assert.Contains(logs.String(), "limit_dimension=pack_container_bytes")
	assert.Contains(logs.String(), fmt.Sprintf("pack_bytes=%d", actualPackBytes))
	assert.Contains(logs.String(), fmt.Sprintf("max_pack_bytes=%d", blobstore.MaxMaintenancePackBytes))
	assert.Contains(logs.String(), "withheld_entries=1")
	assert.NotContains(logs.String(), "raw_bytes=", "pack metadata limits are not blob raw sizes")
}

func TestRunSealsWriterBeforeEntryCountLimitIsExceeded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, 2)
	f := newMaintenanceFixture(t)
	for i := range 5 {
		content := fmt.Appendf(nil, "entry-count-%d", i)
		f.addBlob(content, maintenanceCanonical)
	}

	stats, err := Run(context.Background(), f.store, f.dir, Options{TargetSize: 1 << 20})
	require.NoError(err)
	assert.Equal(3, stats.PacksSealed)
	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 3)
	for _, rec := range recs {
		assert.LessOrEqual(rec.EntryCount, int64(2))
	}
}

func TestRunRecoversOversizedIndexedBlobFromLooseCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	legacy := f.write("legacy/"+hash, content)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(stats.BlobsPacked)
	assert.NoFileExists(legacy)
	entry, err = f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.Nil(entry, "unsafe oversized mapping is dropped only after canonical recovery")
	assert.Equal(content, f.read(hash))
}

func TestRunKeepsOversizedIndexWhenLooseRecoveryIsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	legacy := f.write("legacy/"+hash, []byte("corrupt recovery bytes"))
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)

	_, err = Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(entry)
	assert.FileExists(legacy)
	assert.Equal(content, f.read(hash), "the still-readable packed copy remains authoritative")
}

func TestRunReportsDatabaseFailureDuringOversizedSweepRecovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	legacyRel := "legacy/" + hash
	legacy := f.write(legacyRel, content)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachments SET storage_path = ? WHERE content_hash = ?`), legacyRel, hash)
	require.NoError(err)
	failureMessage := installCanonicalizationFailure(t, f.store, failSweepCanonicalization)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)

	_, err = Run(context.Background(), f.store, f.dir, Options{})
	require.ErrorContains(err, failureMessage,
		"a systemic DB failure must not be downgraded to a damaged-content warning")
	assert.FileExists(legacy)
	entry, lookupErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(lookupErr)
	assert.NotNil(entry, "the mapping stays authoritative when canonicalization does not commit")
}

func TestCanonicalizeLooseSourceStopsBeforeDatabaseCommitWhenContextCancels(t *testing.T) {
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("context cancellation after streaming copy")
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	ctx := &cancelAfterErrContext{Context: context.Background(), cancelAfter: 3}

	err := canonicalizeLooseSource(ctx, f.store, f.dir, []string{hash}, mustNormalizedBlobHash(t, hash), legacy)
	require.ErrorIs(t, err, context.Canceled)
	assert.FileExists(legacy, "source deletion must follow a successful DB commit")
	assert.Equal("legacy/"+hash, f.storagePath(hash))
	assert.FileExists(canonicalLoosePath(f.dir, mustNormalizedBlobHash(t, hash)),
		"a fully published canonical copy is harmless recovery data")
}

func TestPublishLooseNeverClobbersRacingDestination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "blob.staging")
	canonical := filepath.Join(dir, "canonical")
	require.NoError(os.WriteFile(staging, []byte("new canonical bytes"), 0o600))
	oldLink := linkLooseFile
	linkLooseFile = func(_, name string) error {
		require.Equal(canonical, name)
		require.NoError(os.WriteFile(canonical, []byte("racing writer bytes"), 0o600))
		return fs.ErrExist
	}
	t.Cleanup(func() { linkLooseFile = oldLink })

	err := publishLooseNoClobber(staging, canonical)
	require.ErrorIs(err, fs.ErrExist)
	got, err := os.ReadFile(canonical)
	require.NoError(err)
	assert.Equal([]byte("racing writer bytes"), got)
	assert.FileExists(staging, "failed publication retains the verified staging source")
}

func TestRunFailsClosedWhenAtomicNoReplacePublishIsUnsupported(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := canonicalLoosePath(f.dir, mustNormalizedBlobHash(t, hash))
	oldLink := linkLooseFile
	linkLooseFile = func(_, _ string) error { return errors.New("atomic no-replace unavailable") }
	t.Cleanup(func() { linkLooseFile = oldLink })

	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.ErrorContains(err, "atomic no-replace unavailable")
	assert.NoFileExists(canonical, "failed publication must never expose a partial final pathname")
	assert.FileExists(legacy)
	assert.Equal("legacy/"+hash, f.storagePath(hash))
	legacyBytes, readErr := os.ReadFile(legacy)
	require.NoError(readErr)
	assert.Equal(content, legacyBytes)
	stagingFiles, globErr := filepath.Glob(filepath.Join(filepath.Dir(canonical), ".*.staging"))
	require.NoError(globErr)
	assert.Empty(stagingFiles, "failed publication cleans only its private same-directory staging file")
}

func TestRunRejectsCanonicalSymlinkToLegacySource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash)))
	require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
	if err := os.Symlink(legacy, canonical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.Error(err)
	assert.FileExists(legacy)
	assert.Equal("legacy/"+hash, f.storagePath(hash))
	info, lstatErr := os.Lstat(canonical)
	require.NoError(lstatErr)
	assert.NotZero(info.Mode() & os.ModeSymlink)
}

func TestRunNeverRemovesLegacyAliasOfCanonicalObject(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash)))
	require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
	if err := os.Link(legacy, canonical); err != nil {
		t.Skipf("hard-link identity fixture unavailable: %v", err)
	}

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.FileExists(legacy, "an alias of canonical storage must never be removed")
	assert.FileExists(canonical)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	legacyInfo, err := os.Stat(legacy)
	require.NoError(err)
	canonicalInfo, err := os.Stat(canonical)
	require.NoError(err)
	assert.True(os.SameFile(legacyInfo, canonicalInfo))
}

func TestRunPreservesInjectedCaseAliasIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := f.write(maintenanceCanonical(hash), content)
	oldSameFile := sameLooseFile
	sameLooseFile = func(_, _ fs.FileInfo) bool { return true }
	t.Cleanup(func() { sameLooseFile = oldSameFile })

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.FileExists(legacy, "case aliases of canonical storage must be preserved")
	assert.FileExists(canonical)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
}

func TestRunSyncsExistingCanonicalDirectoryBeforeCommittingLegacyMigration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := f.write(maintenanceCanonical(hash), content)
	oldSyncDir := pack.SyncDir
	calls := 0
	pack.SyncDir = func(path string) error {
		calls++
		if calls == 1 {
			return errors.New("injected canonical directory sync failure")
		}
		return oldSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = oldSyncDir })

	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.ErrorContains(err, "injected canonical directory sync failure")
	assert.FileExists(legacy)
	assert.FileExists(canonical)
	assert.Equal("legacy/"+hash, f.storagePath(hash))

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.NoFileExists(legacy)
	assert.FileExists(canonical)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
}

func TestRunPacksAndNormalizesUppercaseAttachmentHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("uppercase attachment hash content")
	hash := maintenanceHash(content)
	uppercase := strings.ToUpper(hash)
	uppercasePath := uppercase[:2] + "/" + uppercase
	f.addRow(uppercase, uppercasePath, len(content))
	f.write(uppercasePath, content)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(content, f.read(hash))
}

func TestRunNormalizesUppercaseAliasWhenOversizedBlobStaysLoose(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := bytes.Repeat([]byte("x"), 1025)
	hash := maintenanceHash(content)
	uppercase := strings.ToUpper(hash)
	legacyPath := "legacy/" + uppercase
	f.addRow(uppercase, legacyPath, len(content))
	legacy := f.write(legacyPath, content)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	assert.NoFileExists(legacy)
	assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash))))
	assert.Equal(content, f.read(hash))
}

func TestRunPreservesMalformedOversizedAttachmentWithoutPanic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	malformed := "x"
	path := "legacy/" + malformed
	f.addRow(malformed, path, 1025)
	full := f.write(path, make([]byte, 1025))

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(full)
	assert.Equal(path, f.storagePath(malformed))
}

func TestRunNormalizesUppercaseReferenceBeforeLooseSweep(t *testing.T) {
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := []byte("lowercase canonical file protected by uppercase reference")
	hash := maintenanceHash(content)
	uppercase := strings.ToUpper(hash)
	f.addRow(uppercase, "missing/"+uppercase, len(content))
	canonical := f.write(maintenanceCanonical(hash), content)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Zero(stats.LooseOrphansRemoved)
	assert.NoFileExists(canonical)
	assert.Equal(content, f.read(hash))
}

func TestRunMalformedReferenceSuppressesUnrelatedLooseDeletion(t *testing.T) {
	f := newMaintenanceFixture(t)
	f.addRow("malformed", "missing/malformed", 10)
	unrelated := []byte("unrelated unreferenced loose content")
	unrelatedHash := maintenanceHash(unrelated)
	unrelatedPath := f.write(maintenanceCanonical(unrelatedHash), unrelated)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(t, err)
	assert.Zero(t, stats.LooseOrphansRemoved)
	assert.FileExists(t, unrelatedPath)
}

func TestRestoreBlobRejectsMalformedHashWithoutDerivingPath(t *testing.T) {
	dir := t.TempDir()
	err := restoreBlob(dir, "x", nil)
	require.Error(t, err)
	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}
