package packer_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// fixture seeds one message in a test store and provides helpers to create
// attachment rows with matching loose blob files under a temp attachments dir.
type fixture struct {
	t     *testing.T
	store *store.Store
	dir   string
	msgID int64
	seq   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-pack", "Pack Thread")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "pack-msg",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage")
	return &fixture{t: t, store: st, dir: t.TempDir(), msgID: msgID}
}

// randomContent returns n incompressible bytes so pack sizes track raw sizes.
func randomContent(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := crand.Read(b)
	require.NoError(t, err, "rand.Read")
	return b
}

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func canonical(hash string) string { return hash[:2] + "/" + hash }

// addRow inserts an attachment row recording hash at the given relative path.
func (f *fixture) addRow(hash, relPath string, size int) {
	f.t.Helper()
	f.seq++
	err := f.store.UpsertAttachment(f.msgID, fmt.Sprintf("file-%d.bin", f.seq),
		"application/octet-stream", relPath, hash, size)
	require.NoErrorf(f.t, err, "UpsertAttachment(%s)", hash)
}

// writeLoose writes content at the slash-separated relPath under the
// attachments dir.
func (f *fixture) writeLoose(relPath string, content []byte) {
	f.t.Helper()
	full := filepath.Join(f.dir, filepath.FromSlash(relPath))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o700), "mkdir loose dir")
	require.NoError(f.t, os.WriteFile(full, content, 0o600), "write loose file")
}

// addBlob records an attachment row and writes the matching loose file at the
// path derived by pathOf; it returns the content hash.
func (f *fixture) addBlob(content []byte, pathOf func(hash string) string) string {
	f.t.Helper()
	h := hashOf(content)
	p := pathOf(h)
	f.addRow(h, p, len(content))
	f.writeLoose(p, content)
	return h
}

// setThumbnail sets thumbnail columns on the newest row with contentHash (no
// public API writes thumbnails yet, so those are set with direct SQL).
func (f *fixture) setThumbnail(contentHash, thumbHash, thumbPath string) {
	f.t.Helper()
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ?
		WHERE id = (SELECT MAX(id) FROM attachments WHERE content_hash = ?)`),
		thumbHash, thumbPath, contentHash)
	require.NoErrorf(f.t, err, "setThumbnail(%s)", thumbHash)
}

func (f *fixture) run(opts packer.Options) packer.Stats {
	f.t.Helper()
	stats, err := packer.Run(context.Background(), f.store, f.dir, opts)
	require.NoError(f.t, err, "packer.Run")
	return stats
}

// readBack reads a blob through the production read path.
func (f *fixture) readBack(hash string) []byte {
	f.t.Helper()
	require := require.New(f.t)
	bs := blobstore.New(f.store, f.dir)
	defer bs.Close() //nolint:errcheck // test cleanup
	r, _, err := bs.Open(hash)
	require.NoErrorf(err, "blobstore.Open(%s)", hash)
	defer r.Close() //nolint:errcheck // test cleanup
	data, err := io.ReadAll(r)
	require.NoError(err, "read blob")
	return data
}

// looseFiles returns the relative paths of all regular files under the
// attachments dir, excluding the packs subtree.
func (f *fixture) looseFiles() []string {
	f.t.Helper()
	packsDir := filepath.Join(f.dir, "packs")
	var files []string
	err := filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == packsDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(f.dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(f.t, err, "walk attachments dir")
	return files
}

func (f *fixture) storagePaths(contentHash string) []string {
	f.t.Helper()
	require := require.New(f.t)
	rows, err := f.store.DB().Query(f.store.Rebind(`
		SELECT storage_path FROM attachments WHERE content_hash = ? ORDER BY id`), contentHash)
	require.NoError(err, "query storage_path")
	defer rows.Close() //nolint:errcheck // read-only cursor
	var paths []string
	for rows.Next() {
		var p string
		require.NoError(rows.Scan(&p), "scan storage_path")
		paths = append(paths, p)
	}
	require.NoError(rows.Err(), "rows.Err")
	return paths
}

func (f *fixture) thumbnailPath(thumbHash string) string {
	f.t.Helper()
	var p string
	err := f.store.DB().QueryRow(f.store.Rebind(`
		SELECT thumbnail_path FROM attachments WHERE thumbnail_hash = ?`), thumbHash).Scan(&p)
	require.NoErrorf(f.t, err, "query thumbnail_path for %s", thumbHash)
	return p
}

// buildOrphanPack seals a pack containing the given blobs directly into
// packs/<id[:2]>/ with no DB record, simulating a crash between seal and
// commit. It returns the pack ID.
func buildOrphanPack(t *testing.T, attachmentsDir string, blobs ...[]byte) string {
	t.Helper()
	require := require.New(t)
	packsDir := filepath.Join(attachmentsDir, "packs")
	require.NoError(os.MkdirAll(packsDir, 0o700), "mkdir packs dir")
	w, err := pack.NewWriter(packsDir, pack.WriterOptions{ZstdLevel: pack.DefaultZstdLevel})
	require.NoError(err, "pack.NewWriter")
	for _, b := range blobs {
		_, err := w.Append(b)
		require.NoError(err, "pack append")
	}
	id := w.ID()
	_, err = w.Seal(filepath.Join(packsDir, id[:2], id+blobstore.PackExt))
	require.NoError(err, "pack seal")
	return id
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestRunPacksLooseBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	var contents [][]byte
	var hashes []string
	for range 4 {
		c := randomContent(t, 600)
		contents = append(contents, c)
		hashes = append(hashes, f.addBlob(c, canonical))
	}
	ncContent := randomContent(t, 600)
	ncHash := f.addBlob(ncContent, func(h string) string {
		return "synctech-sms/" + h[:2] + "/" + h
	})
	contents = append(contents, ncContent)
	hashes = append(hashes, ncHash)

	thumbContent := randomContent(t, 200)
	thumbHash := hashOf(thumbContent)
	f.writeLoose("thumbs/"+thumbHash, thumbContent)
	f.setThumbnail(hashes[0], thumbHash, "thumbs/"+thumbHash)
	contents = append(contents, thumbContent)
	hashes = append(hashes, thumbHash)

	stats := f.run(packer.Options{TargetSize: 1024})

	assert.Equal(3, stats.PacksSealed, "1024-byte target packs two 600-byte blobs per pack")
	assert.Equal(6, stats.BlobsPacked)
	assert.Equal(int64(5*600+200), stats.BytesPacked)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksRemoved)
	assert.Zero(stats.BlobsMissing)
	assert.Zero(stats.BlobsCorrupt)

	for i, h := range hashes {
		entry, err := f.store.GetAttachmentPackEntry(h)
		require.NoErrorf(err, "GetAttachmentPackEntry(%s)", h)
		require.NotNilf(entry, "blob %s must be indexed", h)
		assert.Equalf(contents[i], f.readBack(h), "blob %s reads back byte-identical", h)
	}

	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 3)
	for _, r := range recs {
		_, err := os.Stat(filepath.Join(f.dir, "packs", r.PackID[:2], r.PackID+blobstore.PackExt))
		require.NoErrorf(err, "pack file for %s exists under packs/<id[:2]>/", r.PackID)
	}

	assert.Empty(f.looseFiles(), "canonical and noncanonical loose files are gone")
	assert.Equal([]string{canonical(ncHash)}, f.storagePaths(ncHash),
		"noncanonical storage_path is canonicalized")
	assert.Equal(canonical(thumbHash), f.thumbnailPath(thumbHash),
		"thumbnail_path is canonicalized")
}

func TestRunHonorsSoftRawByteBudget(t *testing.T) {
	tests := []struct {
		name            string
		maxBytes        int64
		wantBlobsPacked int
		wantBytesPacked int64
		wantExhausted   bool
	}{
		{
			name:            "below first blob boundary still makes progress",
			maxBytes:        99,
			wantBlobsPacked: 1,
			wantBytesPacked: 100,
			wantExhausted:   true,
		},
		{
			name:            "exact first blob boundary",
			maxBytes:        100,
			wantBlobsPacked: 1,
			wantBytesPacked: 100,
			wantExhausted:   true,
		},
		{
			name:            "above first blob boundary advances through second blob",
			maxBytes:        101,
			wantBlobsPacked: 2,
			wantBytesPacked: 200,
			wantExhausted:   true,
		},
		{
			name:            "zero is unlimited",
			maxBytes:        0,
			wantBlobsPacked: 3,
			wantBytesPacked: 300,
			wantExhausted:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)

			contents := make([][]byte, 0, 3)
			hashes := make([]string, 0, 3)
			for range 3 {
				content := randomContent(t, 100)
				contents = append(contents, content)
				hashes = append(hashes, f.addBlob(content, canonical))
			}

			stats := f.run(packer.Options{MaxBytes: tt.maxBytes})

			assert.Equal(tt.wantBlobsPacked, stats.BlobsPacked)
			assert.Equal(tt.wantBytesPacked, stats.BytesPacked)
			assert.Equal(tt.wantExhausted, stats.BudgetExhausted)
			if tt.wantBlobsPacked > 0 {
				assert.Equal(1, stats.PacksSealed, "budget stop seals the current partial writer")
			}

			for i, hash := range hashes {
				entry, err := f.store.GetAttachmentPackEntry(hash)
				require.NoError(err, "GetAttachmentPackEntry(%s)", hash)
				if i < tt.wantBlobsPacked {
					require.NotNil(entry, "packed blob %d must be indexed", i)
					assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(hash))),
						"packed blob %d loose file", i)
				} else {
					assert.Nil(entry, "blob %d beyond the budget must remain loose", i)
					assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(hash))),
						"remaining loose blob %d", i)
				}
				assert.Equal(contents[i], f.readBack(hash), "blob %d stays readable", i)
			}
		})
	}
}

func TestRunBudgetExhaustionStillCompletesRecoveryAndSweep(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	danglingContent := randomContent(t, 600)
	danglingHash := f.addBlob(danglingContent, canonical)
	sweepContent := randomContent(t, 600)
	sweepHash := f.addBlob(sweepContent, canonical)
	first := f.run(packer.Options{TargetSize: 600})
	require.Equal(2, first.PacksSealed, "fixture requires separate pack files")

	danglingEntry, err := f.store.GetAttachmentPackEntry(danglingHash)
	require.NoError(err)
	require.NotNil(danglingEntry)
	require.NoError(os.Remove(filepath.Join(
		f.dir, "packs", danglingEntry.PackID[:2], danglingEntry.PackID+blobstore.PackExt)))
	f.writeLoose(canonical(danglingHash), danglingContent)
	f.writeLoose(canonical(sweepHash), sweepContent)

	orphanContent := randomContent(t, 300)
	orphanHash := f.addBlob(orphanContent, canonical)
	orphanID := buildOrphanPack(t, f.dir, orphanContent)

	remainingContent := randomContent(t, 100)
	remainingHash := f.addBlob(remainingContent, canonical)

	stats := f.run(packer.Options{MaxBytes: 100})

	assert.True(stats.BudgetExhausted)
	assert.Equal(1, stats.RecordsDropped, "dangling metadata repair still runs")
	assert.Equal(1, stats.PacksAdopted, "orphan reconciliation still runs")
	assert.Equal(1, stats.BlobsPacked, "soft budget makes progress by repairing one loose blob")
	assert.Equal(int64(600), stats.BytesPacked)
	assert.Equal(2, stats.LooseSwept, "final sweep removes adopted and already-indexed leftovers")

	hasOrphan, err := f.store.HasPackRecord(orphanID)
	require.NoError(err)
	assert.True(hasOrphan)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(sweepHash))))
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(orphanHash))))
	assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(remainingHash))))

	for hash, content := range map[string][]byte{
		danglingHash:  danglingContent,
		sweepHash:     sweepContent,
		orphanHash:    orphanContent,
		remainingHash: remainingContent,
	} {
		assert.Equal(content, f.readBack(hash), "blob %s stays readable", hash)
	}
}

func TestRunTriesAllRecordedPathsForSameHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := hashOf(content)
	f.addRow(hash, "00-missing/"+hash, len(content))
	src, err := f.store.GetSourceByIdentifier("alice@example.com")
	require.NoError(err)
	convID, err := f.store.EnsureConversation(src.ID, "thread-pack", "Pack Thread")
	require.NoError(err)
	msgID, err := f.store.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID,
		SourceMessageID: "pack-msg-second-path", MessageType: "email",
	})
	require.NoError(err)
	validPath := "zz-valid/" + hash
	require.NoError(f.store.UpsertAttachment(msgID, "valid.bin", "application/octet-stream",
		validPath, hash, len(content)))
	f.writeLoose(validPath, content)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.BlobsPacked, "valid alternate path must be packed")
	assert.Zero(stats.BlobsMissing, "one missing candidate does not make the blob missing")
	assert.Equal(content, f.readBack(hash))
}

func TestRunAlreadyCanceledDoesNotMutatePackDirectory(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := packer.Run(ctx, f.store, f.dir, packer.Options{})
	require.ErrorIs(err, context.Canceled)
	_, err = os.Stat(filepath.Join(f.dir, "packs"))
	require.ErrorIs(err, os.ErrNotExist,
		"pre-canceled run must return before creating or reconciling the pack directory")
}

func TestRunSecondRunIsNoOp(t *testing.T) {
	assert := assert.New(t)
	f := newFixture(t)
	f.addBlob(randomContent(t, 600), canonical)
	f.addBlob(randomContent(t, 600), canonical)

	first := f.run(packer.Options{})
	assert.Equal(1, first.PacksSealed)
	assert.Equal(2, first.BlobsPacked)

	second := f.run(packer.Options{})
	assert.Equal(packer.Stats{}, second, "second run is a no-op")
}

func TestRunAdoptsOrphanPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c1 := randomContent(t, 600)
	c2 := randomContent(t, 600)
	h1 := f.addBlob(c1, canonical)
	h2 := f.addBlob(c2, canonical)
	id := buildOrphanPack(t, f.dir, c1, c2)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.PacksAdopted)
	assert.Zero(stats.PacksSealed, "adopted blobs are not re-packed")
	assert.Zero(stats.BlobsPacked)
	assert.Equal(2, stats.LooseSwept, "loose files of adopted blobs are swept")

	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.True(has, "adoption records the pack")
	for h, want := range map[string][]byte{h1: c1, h2: c2} {
		entry, err := f.store.GetAttachmentPackEntry(h)
		require.NoErrorf(err, "GetAttachmentPackEntry(%s)", h)
		require.NotNilf(entry, "blob %s indexed by adoption", h)
		assert.Equal(id, entry.PackID)
		assert.Equal(want, f.readBack(h))
	}
	assert.Empty(f.looseFiles())
}

func TestRunAdoptsOnlyReferencedOrphanEntries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	liveContent := randomContent(t, 600)
	deadContent := randomContent(t, 600)
	liveHash := hashOf(liveContent)
	deadHash := hashOf(deadContent)
	f.addRow(liveHash, canonical(liveHash), len(liveContent))
	id := buildOrphanPack(t, f.dir, liveContent, deadContent)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.PacksAdopted)
	liveEntry, err := f.store.GetAttachmentPackEntry(liveHash)
	require.NoError(err)
	require.NotNil(liveEntry)
	assert.Equal(id, liveEntry.PackID)
	deadEntry, err := f.store.GetAttachmentPackEntry(deadHash)
	require.NoError(err)
	assert.Nil(deadEntry, "dead footer entry must never be resurrected")
	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 1)
	assert.Equal(int64(2), recs[0].EntryCount,
		"immutable pack record retains full footer accounting")
}

func TestRunSkipsMislocatedPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 600)
	h := f.addBlob(c, canonical)

	// Seal a valid pack containing the blob directly under packs/ instead of
	// the sharded packs/<id[:2]>/ path readers construct. Reconciliation must
	// not adopt it: doing so would index a blob the blob store cannot open,
	// and the sweep would then delete its loose copy.
	packsDir := filepath.Join(f.dir, "packs")
	require.NoError(os.MkdirAll(packsDir, 0o700), "mkdir packs dir")
	w, err := pack.NewWriter(packsDir, pack.WriterOptions{ZstdLevel: pack.DefaultZstdLevel})
	require.NoError(err, "pack.NewWriter")
	_, err = w.Append(c)
	require.NoError(err, "pack append")
	mislocatedID := w.ID()
	mislocatedPath := filepath.Join(packsDir, mislocatedID+blobstore.PackExt)
	_, err = w.Seal(mislocatedPath)
	require.NoError(err, "pack seal")

	stats := f.run(packer.Options{})

	assert.Zero(stats.PacksAdopted, "mislocated pack is not adopted")
	assert.Equal(1, stats.PacksSealed, "loose blob is packed normally instead")
	has, err := f.store.HasPackRecord(mislocatedID)
	require.NoError(err)
	assert.False(has, "no record for the mislocated pack")
	_, err = os.Stat(mislocatedPath)
	require.NoError(err, "mislocated pack file is left in place")

	entry, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	require.NotNil(entry, "blob indexed via a normally sealed pack")
	assert.NotEqual(mislocatedID, entry.PackID)
	assert.Equal(c, f.readBack(h))
}

func TestRunRemovesRedundantOrphanPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 600)
	h := f.addBlob(c, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)

	entryBefore, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	require.NotNil(entryBefore)

	orphanID := buildOrphanPack(t, f.dir, c)
	second := f.run(packer.Options{})

	assert.Equal(1, second.PacksRemoved)
	assert.Zero(second.PacksAdopted)
	_, err = os.Stat(filepath.Join(f.dir, "packs", orphanID[:2], orphanID+blobstore.PackExt))
	require.ErrorIs(err, os.ErrNotExist, "redundant orphan pack file is deleted")
	has, err := f.store.HasPackRecord(orphanID)
	require.NoError(err)
	assert.False(has, "no record for the removed orphan")

	entryAfter, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	require.NotNil(entryAfter)
	assert.Equal(entryBefore.PackID, entryAfter.PackID, "index still points at the original pack")
	assert.Equal(c, f.readBack(h))
}

func TestRunAdoptsOrphanForUppercaseReferenceAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := hashOf(content)
	uppercase := strings.ToUpper(hash)
	f.addRow(uppercase, "legacy/"+uppercase, len(content))
	orphanID := buildOrphanPack(t, f.dir, content)
	orphanPath := filepath.Join(f.dir, "packs", orphanID[:2], orphanID+blobstore.PackExt)

	first := f.run(packer.Options{})

	assert.Equal(1, first.PacksAdopted)
	assert.Zero(first.PacksRemoved)
	assert.Equal([]string{canonical(hash)}, f.storagePaths(hash),
		"adoption must normalize the attachment reference in its index transaction")
	assert.Empty(f.storagePaths(uppercase))
	assert.Equal(content, f.readBack(hash))

	usage, err := f.store.ListPackUsage(context.Background())
	require.NoError(err)
	require.Len(usage, 1)
	assert.Equal(orphanID, usage[0].PackID)
	assert.Equal(int64(1), usage[0].LiveEntries,
		"the adopted alias must count as live for repack accounting")
	live, err := f.store.ListReferencedPackEntries(context.Background(), orphanID)
	require.NoError(err)
	assert.Len(live, 1)

	second := f.run(packer.Options{})

	assert.Zero(second.MappingsPruned)
	assert.Zero(second.PacksRemoved)
	assert.FileExists(orphanPath)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(orphanID, entry.PackID)
	assert.Equal(content, f.readBack(hash))
}

func TestRunPreservesUppercaseExternalReferencesToAdoptedOrphans(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(string) string
	}{
		{name: "URL only", path: func(hash string) string {
			return "HTTPS://cdn.example.com/" + hash
		}},
		{name: "empty path only", path: func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)
			if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
				f.store.DB().SetMaxOpenConns(1)
				f.store.DB().SetMaxIdleConns(1)
				_, err := f.store.DB().Exec(`PRAGMA case_sensitive_like = ON`)
				require.NoError(err)
			}

			content := randomContent(t, 600)
			hash := hashOf(content)
			uppercase := strings.ToUpper(hash)
			preservedPath := tc.path(uppercase)
			f.addRow(uppercase, preservedPath, len(content))
			orphanID := buildOrphanPack(t, f.dir, content)
			orphanPath := filepath.Join(f.dir, "packs", orphanID[:2], orphanID+blobstore.PackExt)

			first := f.run(packer.Options{})

			assert.Equal(1, first.PacksAdopted)
			assert.Equal([]string{preservedPath}, f.storagePaths(uppercase),
				"external metadata policy must preserve the original hash and path")
			assert.Empty(f.storagePaths(hash))
			for _, requested := range []string{hash, uppercase} {
				assert.Equal(content, f.readBack(requested))
			}
			usage, err := f.store.ListPackUsage(context.Background())
			require.NoError(err)
			require.Len(usage, 1)
			assert.Equal(int64(1), usage[0].LiveEntries)
			live, err := f.store.ListReferencedPackEntries(context.Background(), orphanID)
			require.NoError(err)
			assert.Len(live, 1)

			second := f.run(packer.Options{})

			assert.Zero(second.MappingsPruned)
			assert.Zero(second.PacksRemoved)
			assert.FileExists(orphanPath)
			entry, err := f.store.GetAttachmentPackEntry(hash)
			require.NoError(err)
			require.NotNil(entry)
			assert.Equal(orphanID, entry.PackID)
		})
	}
}

func TestRunCoalescesSimultaneousLooseCaseAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		f.store.DB().SetMaxOpenConns(1)
		f.store.DB().SetMaxIdleConns(1)
		_, err := f.store.DB().Exec(`PRAGMA case_sensitive_like = ON`)
		require.NoError(err)
	}

	sharedContent := randomContent(t, 900)
	sharedHash := hashOf(sharedContent)
	uppercase := strings.ToUpper(sharedHash)
	upperPath := "legacy/" + uppercase
	f.addRow(uppercase, upperPath, len(sharedContent))
	f.writeLoose(upperPath, sharedContent)
	src, err := f.store.GetOrCreateSource("gmail", "case-alias@example.com")
	require.NoError(err)
	convID, err := f.store.EnsureConversation(src.ID, "case-alias-thread", "Case Alias Thread")
	require.NoError(err)
	aliasMsgID, err := f.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "case-alias-message",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err)
	require.NoError(f.store.UpsertAttachment(aliasMsgID, "case-alias.bin", "application/octet-stream",
		canonical(sharedHash), sharedHash, len(sharedContent)))
	f.writeLoose(canonical(sharedHash), sharedContent)
	unrelatedContent := randomContent(t, 700)
	unrelatedHash := f.addBlob(unrelatedContent, canonical)

	first := f.run(packer.Options{})

	assert.Equal(2, first.BlobsPacked, "case aliases append one shared BlobID")
	entry, err := f.store.GetAttachmentPackEntry(sharedHash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	reader, err := pack.OpenReader(packPath, nil)
	require.NoError(err)
	assert.Len(reader.Entries(), 2, "shared aliases and the unrelated blob produce two footer entries")
	require.NoError(reader.Close())

	assert.Equal([]string{canonical(sharedHash), canonical(sharedHash)}, f.storagePaths(sharedHash))
	assert.Empty(f.storagePaths(uppercase))
	assert.Empty(f.looseFiles(), "selected and alias loose sources are swept after verified publication")
	for _, requested := range []string{sharedHash, uppercase} {
		assert.Equal(sharedContent, f.readBack(requested))
		bs := blobstore.New(f.store, f.dir)
		data, size, err := bs.ReadBounded(requested, int64(len(sharedContent)))
		require.NoError(err)
		assert.Equal(int64(len(sharedContent)), size)
		assert.Equal(sharedContent, data)
		require.NoError(bs.Close())
	}
	assert.Equal(unrelatedContent, f.readBack(unrelatedHash))

	second := f.run(packer.Options{})

	assert.Zero(second.BlobsPacked)
	assert.Zero(second.PacksSealed)
	assert.Zero(second.MappingsPruned)
	assert.FileExists(packPath)
	assert.Equal(sharedContent, f.readBack(sharedHash))
}

func TestRunDoesNotDeriveLoosePathForMalformedHash(t *testing.T) {
	assert := assert.New(t)
	f := newFixture(t)
	malformed := "zz" + strings.Repeat("0", 62)
	recordedPath := "missing/" + malformed
	derivedPath := malformed[:2] + "/" + malformed
	f.addRow(malformed, recordedPath, 20)
	f.writeLoose(derivedPath, []byte("must remain unmanaged"))

	stats := f.run(packer.Options{})

	assert.Zero(stats.BlobsPacked)
	assert.Zero(stats.PacksSealed)
	assert.Equal([]string{recordedPath}, f.storagePaths(malformed))
	assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(derivedPath)),
		"malformed hashes must never authorize a derived content-addressed path")
}

func TestRunPrunesTeamsReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	oldContent := randomContent(t, 600)
	oldHash := hashOf(oldContent)
	require.NoError(f.store.ReplaceMessageInlineAttachments(f.msgID, []store.AttachmentRef{{
		StoragePath: canonical(oldHash), ContentHash: oldHash, Size: len(oldContent),
		SourceAttachmentID: "teams:inline:old",
	}}))
	f.writeLoose(canonical(oldHash), oldContent)
	first := f.run(packer.Options{})
	require.Equal(1, first.BlobsPacked)
	oldEntry, err := f.store.GetAttachmentPackEntry(oldHash)
	require.NoError(err)
	require.NotNil(oldEntry)

	newHash := hashOf([]byte("replacement teams inline media"))
	require.NoError(f.store.ReplaceMessageInlineAttachments(f.msgID, []store.AttachmentRef{{
		StoragePath: canonical(newHash), ContentHash: newHash, Size: 30,
		SourceAttachmentID: "teams:inline:new",
	}}))

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.MappingsPruned)
	oldEntry, err = f.store.GetAttachmentPackEntry(oldHash)
	require.NoError(err)
	assert.Nil(oldEntry, "replacement removes the old hash from live pack accounting")
	referenced, err := f.store.ListReferencedBlobHashes()
	require.NoError(err)
	assert.NotContains(referenced, oldHash)
}

func TestRunCleansUnreferencedLoose(t *testing.T) {
	assert := assert.New(t)
	f := newFixture(t)

	canonicalDead := hashOf([]byte("dead canonical loose attachment"))
	legacyDead := hashOf([]byte("dead legacy loose attachment"))
	f.writeLoose(canonical(canonicalDead), []byte("dead canonical loose attachment"))
	f.writeLoose("legacy/"+legacyDead, []byte("dead legacy loose attachment"))

	referenced := hashOf([]byte("referenced loose attachment"))
	// Record a missing legacy candidate so packLoose cannot consume the extra
	// canonical copy; the final classifier must retain it based on liveness.
	f.addRow(referenced, "missing/"+referenced, 27)
	f.writeLoose(canonical(referenced), []byte("referenced loose attachment"))
	f.writeLoose("notes/not-a-hash", []byte("unmanaged file"))
	f.writeLoose("packs/"+hashOf([]byte("inside packs")), []byte("inside packs"))

	stats := f.run(packer.Options{})

	assert.Equal(2, stats.LooseOrphansRemoved)
	assert.NoFileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(canonicalDead))))
	assert.NoFileExists(filepath.Join(f.dir, "legacy", legacyDead))
	assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(canonical(referenced))))
	assert.FileExists(filepath.Join(f.dir, "notes", "not-a-hash"))
	assert.FileExists(filepath.Join(f.dir, "packs", hashOf([]byte("inside packs"))))
}

func TestRunQuarantinesMixedDamagedOrphan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	logs := captureLogs(t)

	valid := randomContent(t, 600)
	damaged := randomContent(t, 600)
	validHash := hashOf(valid)
	damagedHash := hashOf(damaged)
	f.addRow(validHash, canonical(validHash), len(valid))
	f.addRow(damagedHash, canonical(damagedHash), len(damaged))
	id := buildOrphanPack(t, f.dir, valid, damaged)
	packPath := filepath.Join(f.dir, "packs", id[:2], id+blobstore.PackExt)
	r, err := pack.OpenReader(packPath, nil)
	require.NoError(err)
	entries := r.Entries()
	require.Len(entries, 2)
	require.NoError(r.Close())
	pf, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	buf := []byte{0}
	_, err = pf.ReadAt(buf, int64(entries[1].Offset))
	require.NoError(err)
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, int64(entries[1].Offset))
	require.NoError(err)
	require.NoError(pf.Close())

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.PacksQuarantined)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksRemoved)
	assert.FileExists(packPath)
	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.False(has)
	for _, hash := range []string{validHash, damagedHash} {
		entry, err := f.store.GetAttachmentPackEntry(hash)
		require.NoError(err)
		assert.Nil(entry, "quarantine must not partially publish %s", hash)
	}
	assert.Contains(logs.String(), id)
	assert.Contains(logs.String(), "failedEntries=1")
	assert.Contains(logs.String(), "withheldEntries=1")
}

func TestRunReportsUnreadableOrphan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	logs := captureLogs(t)

	id := pack.NewPackID()
	path := filepath.Join(f.dir, "packs", id[:2], id+blobstore.PackExt)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, []byte("not a pack footer"), 0o600))

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.PacksUnreadable)
	assert.Zero(stats.PacksQuarantined)
	assert.FileExists(path)
	assert.Contains(logs.String(), id)
	assert.Contains(logs.String(), path)
	assert.Contains(logs.String(), "error=")
}

// TestRunRepairsDanglingRecordsBeforeReconcilingOrphans pins the ordering
// between the two recovery passes. A stale index row for a missing pack must
// be removed before an orphan pack is classified: otherwise the orphan looks
// redundant, is deleted, and the stale row is then dropped, destroying the
// only readable copy.
func TestRunRepairsDanglingRecordsBeforeReconcilingOrphans(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)

	stale, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(stale)
	require.NoError(os.Remove(filepath.Join(
		f.dir, "packs", stale.PackID[:2], stale.PackID+blobstore.PackExt)))

	orphanID := buildOrphanPack(t, f.dir, content)
	second := f.run(packer.Options{})

	assert.Equal(1, second.RecordsDropped, "missing recorded pack is repaired")
	assert.Equal(1, second.PacksAdopted, "orphan copy is adopted after stale index removal")
	assert.Zero(second.PacksRemoved, "the only readable orphan copy must not be deleted")
	assert.Zero(second.BlobsMissing, "adoption must not fall through to the absent loose file")

	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(orphanID, entry.PackID)
	assert.Equal(content, f.readBack(hash))
}

// TestRunAdoptsOrphanWhenIndexedPackIsUnreadable pins the other orphan
// classification hazard: an index row is not proof that its packed copy is
// readable. A valid orphan must replace an unreadable indexed copy instead of
// being deleted as redundant.
func TestRunAdoptsOrphanWhenIndexedPackIsUnreadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)

	stale, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(stale)
	packPath := filepath.Join(f.dir, "packs", stale.PackID[:2], stale.PackID+blobstore.PackExt)
	pf, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	buf := make([]byte, 1)
	_, err = pf.ReadAt(buf, stale.Offset)
	require.NoError(err)
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, stale.Offset)
	require.NoError(err)
	require.NoError(pf.Close())

	orphanID := buildOrphanPack(t, f.dir, content)
	second := f.run(packer.Options{})

	assert.Equal(1, second.PacksAdopted, "valid orphan replaces unreadable indexed copy")
	assert.Zero(second.PacksRemoved, "valid orphan must not be classified as redundant")
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(orphanID, entry.PackID)
	assert.Equal(content, f.readBack(hash))
}

// TestRunDropsDanglingPackRecords pins the restored-vault self-heal: backup
// restore materializes loose files but no production packs, so a restored DB
// can carry pack rows whose files do not exist. The packer must drop those
// rows (so the blobs re-pack from loose) instead of skipping them as "already
// indexed" while the sweep deletes their only loose copies.
func TestRunDropsDanglingPackRecords(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c1 := randomContent(t, 600)
	c2 := randomContent(t, 600)
	h1 := f.addBlob(c1, canonical)
	h2 := f.addBlob(c2, canonical)

	first := f.run(packer.Options{TargetSize: 600}) // one blob per pack
	require.Equal(2, first.PacksSealed)
	require.Zero(first.RecordsDropped)
	require.Empty(f.looseFiles(), "first run's sweep removed only what it packed")
	require.Equal(c2, f.readBack(h2), "first run left the other pack's blob readable")

	// Simulate the restored-vault state: pack file gone, DB rows still
	// present, blob bytes back as a loose canonical file (what restore
	// materializes).
	entry1, err := f.store.GetAttachmentPackEntry(h1)
	require.NoError(err)
	require.NotNil(entry1)
	danglingID := entry1.PackID
	require.NoError(os.Remove(
		filepath.Join(f.dir, "packs", danglingID[:2], danglingID+blobstore.PackExt)))
	f.writeLoose(canonical(h1), c1)

	second := f.run(packer.Options{})

	assert.Equal(1, second.RecordsDropped, "dangling pack record dropped")
	assert.Equal(1, second.PacksSealed, "blob re-packed from its loose copy")
	assert.Equal(1, second.BlobsPacked)

	has, err := f.store.HasPackRecord(danglingID)
	require.NoError(err)
	assert.False(has, "dangling attachment_packs row removed")

	entryAfter, err := f.store.GetAttachmentPackEntry(h1)
	require.NoError(err)
	require.NotNil(entryAfter, "blob re-indexed")
	assert.NotEqual(danglingID, entryAfter.PackID, "index points at the new pack")
	assert.Equal(c1, f.readBack(h1), "re-packed blob readable via blobstore")
	assert.Equal(c2, f.readBack(h2), "untouched pack unaffected")
	assert.Empty(f.looseFiles(), "loose copy removed after re-packing")
}

// TestRunRemovesFullyUnreferencedOrphan pins that dead entries are never
// verified or adopted merely because an orphan footer names them.
func TestRunRemovesFullyUnreferencedOrphan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 600)
	id := buildOrphanPack(t, f.dir, c) // blob is unindexed: no DB rows exist
	packPath := filepath.Join(f.dir, "packs", id[:2], id+blobstore.PackExt)

	// Flip one byte early in the blob data region (past the header, well
	// before the footer) so ReadBlob's CRC check fails while the SHA-verified
	// footer still parses and reconciliation reaches verification.
	pf, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err, "open pack file for corruption")
	buf := make([]byte, 1)
	_, err = pf.ReadAt(buf, 10)
	require.NoError(err, "read byte to corrupt")
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, 10)
	require.NoError(err, "write corrupted byte")
	require.NoError(pf.Close(), "close pack file")

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.PacksRemoved, "fully dead orphan is redundant without reading blob bytes")
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksQuarantined)
	_, err = os.Stat(packPath)
	require.ErrorIs(err, os.ErrNotExist)
	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.False(has, "no record for the unadoptable pack")
}

func TestRunSweepsIndexedLooseLeftovers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 600)
	h := f.addBlob(c, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)
	require.Empty(f.looseFiles())

	// Recreate the source after a real pack and index exist, simulating a crash
	// after the pack transaction committed but before source deletion.
	f.writeLoose(canonical(h), c)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.LooseSwept)
	assert.Zero(stats.PacksSealed)
	assert.Zero(stats.BlobsPacked)
	assert.Empty(f.looseFiles(), "lingering loose file removed by the sweep")
}

func TestRunPreservesLooseCopyWhenIndexedPackIsUnreadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)

	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	pf, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	buf := make([]byte, 1)
	_, err = pf.ReadAt(buf, entry.Offset)
	require.NoError(err)
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, entry.Offset)
	require.NoError(err)
	require.NoError(pf.Close())

	// Simulate an indexed loose leftover. The corrupt pack is not a readable
	// authoritative copy, so the sweep must preserve these verified bytes.
	f.writeLoose(canonical(hash), content)
	stats := f.run(packer.Options{})

	assert.Zero(stats.LooseSwept)
	loose, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(canonical(hash))))
	require.NoError(err, "loose recovery copy must remain")
	assert.Equal(content, loose)
	entry, err = f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.Nil(entry, "unreadable packed index is dropped after the loose copy verifies")
	assert.Equal(content, f.readBack(hash),
		"production reads must recover immediately through canonical loose fallback")
}

func TestRunKeepsPackedIndexWhenLooseRecoveryCopyIsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	first := f.run(packer.Options{})
	require.Equal(1, first.PacksSealed)

	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	packPath := filepath.Join(f.dir, "packs", entry.PackID[:2], entry.PackID+blobstore.PackExt)
	pf, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	buf := make([]byte, 1)
	_, err = pf.ReadAt(buf, entry.Offset)
	require.NoError(err)
	buf[0] ^= 0xff
	_, err = pf.WriteAt(buf, entry.Offset)
	require.NoError(err)
	require.NoError(pf.Close())
	f.writeLoose(canonical(hash), []byte("also corrupt"))

	stats := f.run(packer.Options{})

	assert.Zero(stats.LooseSwept)
	entry, err = f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(entry, "unverified loose bytes must not become the read fallback")
	loose, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(canonical(hash))))
	require.NoError(err)
	assert.Equal([]byte("also corrupt"), loose, "both damaged copies remain available for repair")
	bs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(bs.Close()) }()
	_, _, err = bs.Open(hash)
	require.Error(err, "reads fail closed while neither copy verifies")
}

func TestRunDropsMalformedPackMetadataBeforeSweepingLooseFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	content := randomContent(t, 600)
	hash := f.addBlob(content, canonical)
	const invalidPackID = "01hzy3v7q8r9s0t1u2v3w4x5y6"

	// Seed malformed restored/corrupt metadata directly, bypassing
	// RecordPackedBlobs' input validation. The loose file is the only readable
	// copy and must not be swept merely because an index row exists.
	_, err := f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 1, 600, ?)`), invalidPackID, "2026-07-09T12:00:00Z")
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO attachment_pack_index
		    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, 6, 600, 600, 0, 0)`), hash, invalidPackID)
	require.NoError(err)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.RecordsDropped, "malformed metadata is removed before enumeration")
	assert.Equal(1, stats.PacksSealed, "the readable loose blob is packed normally")
	assert.Equal(1, stats.BlobsPacked)
	assert.Zero(stats.LooseSwept, "the loose copy is removed only by a successful pack commit")
	assert.Equal(content, f.readBack(hash))
	has, err := f.store.HasPackRecord(invalidPackID)
	require.NoError(err)
	assert.False(has)
}

func TestRunRemovesStaleStagingFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	packsDir := filepath.Join(f.dir, "packs")
	require.NoError(os.MkdirAll(packsDir, 0o700))
	staging := filepath.Join(packsDir, "01hzy3v7q8r9s0t1u2v3w4x5y6.staging")
	require.NoError(os.WriteFile(staging, []byte("dead mid-seal abort"), 0o600))

	stats := f.run(packer.Options{})

	assert.Equal(packer.Stats{}, stats)
	_, err := os.Stat(staging)
	assert.ErrorIs(err, os.ErrNotExist, "stale staging file removed at start")
}

func TestRunCountsMissingSourceFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	missing := randomContent(t, 100)
	missingHash := hashOf(missing)
	f.addRow(missingHash, canonical(missingHash), len(missing))
	good := randomContent(t, 100)
	goodHash := f.addBlob(good, canonical)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.BlobsMissing)
	assert.Equal(1, stats.BlobsPacked, "run continues past the missing blob")
	entry, err := f.store.GetAttachmentPackEntry(missingHash)
	require.NoError(err)
	assert.Nil(entry, "missing blob stays unindexed for backfill")
	entry, err = f.store.GetAttachmentPackEntry(goodHash)
	require.NoError(err)
	assert.NotNil(entry)
}

func TestRunSkipsCorruptSourceFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	corrupt := randomContent(t, 100)
	corruptHash := hashOf(corrupt)
	f.addRow(corruptHash, canonical(corruptHash), len(corrupt))
	f.writeLoose(canonical(corruptHash), []byte("bytes that do not match the hash"))
	good := randomContent(t, 100)
	goodHash := f.addBlob(good, canonical)

	stats := f.run(packer.Options{})

	assert.Equal(1, stats.BlobsCorrupt)
	assert.Equal(1, stats.BlobsPacked, "run continues past the corrupt blob")
	entry, err := f.store.GetAttachmentPackEntry(corruptHash)
	require.NoError(err)
	assert.Nil(entry, "corrupt blob is not indexed")
	_, err = os.Stat(filepath.Join(f.dir, filepath.FromSlash(canonical(corruptHash))))
	require.NoError(err, "corrupt file is left in place")
	entry, err = f.store.GetAttachmentPackEntry(goodHash)
	require.NoError(err)
	assert.NotNil(entry)
}

func TestRunHonorsContextCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)

	c := randomContent(t, 100)
	h := f.addBlob(c, canonical)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := packer.Run(ctx, f.store, f.dir, packer.Options{})
	require.ErrorIs(err, context.Canceled)

	entry, err := f.store.GetAttachmentPackEntry(h)
	require.NoError(err)
	assert.Nil(entry, "nothing indexed after cancellation")
}
