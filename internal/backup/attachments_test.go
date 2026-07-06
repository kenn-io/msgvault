package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

func writeLooseAttachment(t *testing.T, dir string, content []byte) ContentRef {
	t.Helper()
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	sub := filepath.Join(dir, hash[:2])
	require.NoError(t, os.MkdirAll(sub, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sub, hash), content, 0o600))
	return ContentRef{Hash: hash, Size: int64(len(content))}
}

// writeNamespacedAttachment stores content under an importer-style namespaced
// path (like synctech-sms does) and returns a ref carrying that path.
func writeNamespacedAttachment(t *testing.T, dir, namespace string, content []byte) ContentRef {
	t.Helper()
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	rel := filepath.Join(namespace, hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), content, 0o600))
	return ContentRef{Hash: hash, Size: int64(len(content)), StoragePath: rel}
}

func TestAttachmentListRoundTrip(t *testing.T) {
	require := require.New(t)
	refs := []ContentRef{
		{Hash: blobID("z").String(), Size: 10},
		{Hash: blobID("a").String(), Size: 0},
	}
	data, err := EncodeAttachmentList(refs)
	require.NoError(err)
	got, err := DecodeAttachmentList(data)
	require.NoError(err)
	require.Equal(refs, got) // order preserved, not sorted

	for _, mut := range []int{0, 5, len(data) - 1} {
		bad := append([]byte{}, data...)
		bad[mut] ^= 0x01
		_, decErr := DecodeAttachmentList(bad)
		require.Error(decErr, "mutated byte %d", mut)
	}
	_, err = EncodeAttachmentList([]ContentRef{{Hash: "nothex", Size: 1}})
	require.Error(err)
}

func TestCaptureAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()

	refA := writeLooseAttachment(t, dir, []byte("attachment A content"))
	refB := writeLooseAttachment(t, dir, []byte("attachment B content"))
	thumb := writeLooseAttachment(t, dir, []byte("thumbnail bytes"))
	thumb.Size = -1 // thumbnails arrive with unknown size

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	parentSeen := map[string]bool{refA.Hash: true} // A already listed by the parent

	cap1, err := CaptureAttachments(context.Background(), dir, []ContentRef{refA, refB, thumb}, parentSeen, appender, CaptureOptions{})
	require.NoError(err)
	assert.Equal(int64(3), cap1.Blobs)
	assert.Equal(int64(20+20+15), cap1.BlobBytes)
	require.True(cap1.HasNewList)
	require.Len(cap1.NewList, 2) // B and thumb only
	assert.Equal(refB.Hash, cap1.NewList[0].Hash)
	assert.Equal(thumb.Hash, cap1.NewList[1].Hash)
	assert.Equal(int64(15), cap1.NewList[1].Size, "thumbnail size filled from disk")

	packs, entries, err := appender.Finish()
	require.NoError(err)
	require.NotEmpty(packs)

	// The new list blob is stored and decodes back.
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	listData, err := r.ReadBlob(known, cap1.NewListBlob, nil)
	require.NoError(err)
	gotRefs, err := DecodeAttachmentList(listData)
	require.NoError(err)
	assert.Equal(cap1.NewList, gotRefs)

	// LoadListRefs round-trips through the repo.
	refs, seen, err := LoadListRefs(r, known, []string{cap1.NewListBlob.String()}, nil)
	require.NoError(err)
	assert.Equal(cap1.NewList, refs)
	assert.True(seen[refB.Hash])
}

// TestCaptureAttachmentsParallelMatchesSerial pins the capture pipeline's
// contract: any Jobs setting produces the same capture accounting, list
// contents, and readable blobs as a strictly serial run, including dedup
// against blobs the repository already holds (which skip compression via the
// known-set snapshot) and in-order progress reporting.
func TestCaptureAttachmentsParallelMatchesSerial(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()

	var refs []ContentRef
	for i := range 40 {
		content := append(bytes.Repeat([]byte{byte(i)}, 100+i*13), byte(i>>4))
		refs = append(refs, writeLooseAttachment(t, dir, content))
	}
	parentSeen := map[string]bool{refs[3].Hash: true, refs[17].Hash: true}

	run := func(jobs int) (*AttachmentCapture, []int, *Repo, map[pack.BlobID]IndexEntry) {
		r := initTestRepo(t)
		appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
		// Pre-store two blobs so the known-snapshot skip path is exercised.
		for _, i := range []int{5, 29} {
			content, err := os.ReadFile(filepath.Join(dir, refs[i].Hash[:2], refs[i].Hash))
			require.NoError(err)
			_, wrote, err := appender.Add(content)
			require.NoError(err)
			require.True(wrote)
		}
		var doneOrder []int
		got, err := CaptureAttachments(context.Background(), dir, append([]ContentRef{}, refs...), parentSeen, appender, CaptureOptions{
			Jobs:     jobs,
			Progress: func(done, _ int, _ int64) { doneOrder = append(doneOrder, done) },
		})
		require.NoError(err)
		_, entries, err := appender.Finish()
		require.NoError(err)
		known := map[pack.BlobID]IndexEntry{}
		for _, e := range entries {
			known[e.Blob] = e
		}
		return got, doneOrder, r, known
	}

	serial, serialOrder, _, serialKnown := run(1)
	parallel, parallelOrder, parallelRepo, parallelKnown := run(8)

	assert.Equal(serial, parallel, "capture results must not depend on Jobs")
	assert.Equal(serialOrder, parallelOrder, "progress must arrive strictly in ref order")
	require.Len(serialOrder, len(refs))
	assert.Equal(int64(len(refs)), parallel.Blobs)
	assert.Len(parallelKnown, len(serialKnown))

	// Every captured blob reads back with content matching its hash:
	// AddEncoded wrote real frames, and ReadBlob re-verifies CRC, frame
	// decode, and the content hash against the blob ID.
	for _, ref := range refs {
		id, err := pack.ParseBlobID(ref.Hash)
		require.NoError(err)
		content, err := parallelRepo.ReadBlob(parallelKnown, id, nil)
		require.NoError(err)
		sum := sha256.Sum256(content)
		assert.Equal(ref.Hash, hex.EncodeToString(sum[:]))
	}
}

func TestCaptureAttachmentsRejectsCorruptFile(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()

	ref := writeLooseAttachment(t, dir, []byte("original"))
	// Corrupt the file on disk so the re-hash mismatches its name.
	path := filepath.Join(dir, ref.Hash[:2], ref.Hash)
	require.NoError(os.WriteFile(path, []byte("tampered"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	_, err := CaptureAttachments(context.Background(), dir, []ContentRef{ref}, map[string]bool{}, appender, CaptureOptions{})
	require.ErrorContains(err, ref.Hash[:2])
}

// TestCaptureAttachmentsParallelReportsFirstErrorInRefOrder pins error
// determinism: with several failing refs and a parallel pipeline that may
// hit a later failure first, the reported error is still the one a serial
// capture would have hit — the failing ref with the lowest index.
func TestCaptureAttachmentsParallelReportsFirstErrorInRefOrder(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()

	var refs []ContentRef
	for i := range 24 {
		refs = append(refs, writeLooseAttachment(t, dir, bytes.Repeat([]byte{byte(i)}, 64)))
	}
	// Two failures: a corrupt file early and a missing file late.
	corruptAt, missingAt := 4, 20
	require.NoError(os.WriteFile(
		filepath.Join(dir, refs[corruptAt].Hash[:2], refs[corruptAt].Hash), []byte("tampered"), 0o600))
	require.NoError(os.Remove(filepath.Join(dir, refs[missingAt].Hash[:2], refs[missingAt].Hash)))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	defer appender.Abort()
	_, err := CaptureAttachments(context.Background(), dir, refs, map[string]bool{}, appender, CaptureOptions{Jobs: 8})
	require.ErrorContains(err, refs[corruptAt].Hash,
		"the lowest-index failure must win, matching serial semantics")
	require.NotContains(err.Error(), refs[missingAt].Hash)
}

func TestCaptureAttachmentsRejectsMalformedHash(t *testing.T) {
	r := initTestRepo(t)
	dir := t.TempDir()

	cases := []struct {
		name string
		hash string
	}{
		{"empty hash", ""},
		{"single char", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
			_, err := CaptureAttachments(context.Background(), dir, []ContentRef{{Hash: tc.hash, Size: 1}}, map[string]bool{}, appender, CaptureOptions{})
			require.ErrorContains(err, "too short")
		})
	}

	// A malformed hash is an in-order failure like any other: a lower-index
	// missing file must win over a later short hash, matching what a serial
	// capture would hit first.
	t.Run("ordering vs earlier failure", func(t *testing.T) {
		require := require.New(t)
		missing := writeLooseAttachment(t, dir, []byte("goes missing"))
		require.NoError(os.Remove(filepath.Join(dir, missing.Hash[:2], missing.Hash)))
		appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
		defer appender.Abort()
		_, err := CaptureAttachments(context.Background(), dir,
			[]ContentRef{missing, {Hash: "a", Size: 1}},
			map[string]bool{}, appender, CaptureOptions{Jobs: 4})
		require.ErrorContains(err, missing.Hash)
		require.NotContains(err.Error(), "too short")
	})
}

// TestCaptureAttachmentsMemoryBudget pins the byte gate: with a budget
// smaller than any file, every file is admitted alone (the oversized-file
// path), and capture still produces exactly the serial result — including
// when a mid-list failure stops the run, which must not deadlock a
// dispatcher waiting on budget.
func TestCaptureAttachmentsMemoryBudget(t *testing.T) {
	require := require.New(t)
	old := captureMemoryBudget
	captureMemoryBudget = 1
	t.Cleanup(func() { captureMemoryBudget = old })

	dir := t.TempDir()
	var refs []ContentRef
	for i := range 20 {
		refs = append(refs, writeLooseAttachment(t, dir, bytes.Repeat([]byte{byte(i)}, 200+i)))
	}

	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	got, err := CaptureAttachments(context.Background(), dir, append([]ContentRef{}, refs...), map[string]bool{}, appender, CaptureOptions{Jobs: 8})
	require.NoError(err)
	require.Equal(int64(len(refs)), got.Blobs)
	_, _, err = appender.Finish()
	require.NoError(err)

	// Failure mid-list under the same tight budget: the error must surface
	// (in ref order) instead of hanging.
	missingAt := 12
	require.NoError(os.Remove(filepath.Join(dir, refs[missingAt].Hash[:2], refs[missingAt].Hash)))
	failAppender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	defer failAppender.Abort()
	_, err = CaptureAttachments(context.Background(), dir, append([]ContentRef{}, refs...), map[string]bool{}, failAppender, CaptureOptions{Jobs: 8})
	require.ErrorContains(err, refs[missingAt].Hash)
}

// TestCaptureAttachmentsReadsRecordedStoragePath pins that capture opens the
// path the database records, not a path derived from the hash: importers such
// as synctech-sms store attachments under namespaced paths, and those files
// exist nowhere else.
func TestCaptureAttachmentsReadsRecordedStoragePath(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()
	ref := writeNamespacedAttachment(t, dir, "ns-source", []byte("namespaced attachment content"))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	got, err := CaptureAttachments(context.Background(), dir, []ContentRef{ref}, map[string]bool{}, appender, CaptureOptions{})
	require.NoError(err)
	require.Equal(int64(1), got.Blobs)
	_, entries, err := appender.Finish()
	require.NoError(err)

	id, err := pack.ParseBlobID(ref.Hash)
	require.NoError(err)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	content, err := r.ReadBlob(known, id, nil)
	require.NoError(err)
	require.Equal([]byte("namespaced attachment content"), content)
}

func TestCaptureAttachmentsRejectsEscapingStoragePath(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()
	ref := writeLooseAttachment(t, dir, []byte("escape attempt"))

	for _, p := range []string{"../outside", "/etc/passwd", "a/../../outside"} {
		ref.StoragePath = p
		appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
		_, err := CaptureAttachments(context.Background(), dir, []ContentRef{ref}, map[string]bool{}, appender, CaptureOptions{})
		require.ErrorContains(err, "escapes the attachments directory", "path %q", p)
		appender.Abort()
	}
}

func TestCaptureAttachmentsNoNewList(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()
	ref := writeLooseAttachment(t, dir, []byte("stable content"))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	defer appender.Abort()
	got, err := CaptureAttachments(context.Background(), dir, []ContentRef{ref}, map[string]bool{ref.Hash: true}, appender, CaptureOptions{})
	require.NoError(err)
	require.False(got.HasNewList)
	require.Empty(got.NewList)
}
