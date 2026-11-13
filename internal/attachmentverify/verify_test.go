package attachmentverify_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/attachmentverify"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fixture struct {
	t     *testing.T
	st    *store.Store
	root  string
	blobs *attachmentstore.Store
	count int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), root)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(blobs.Close()) })
	return &fixture{t: t, st: st, root: root, blobs: blobs}
}
func (f *fixture) row(hash, path string, size int) (attachmentID, messageID int64) {
	f.t.Helper()
	require := require.New(f.t)
	f.count++
	source, err := f.st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	conv, err := f.st.EnsureConversation(f.t.Context(), source.ID, "synthetic-thread", "Synthetic")
	require.NoError(err)
	messageID, err = f.st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv,
		SourceMessageID: fmt.Sprintf("synthetic-%d", f.count), MessageType: "email"})
	require.NoError(err)
	require.NoError(f.st.UpsertAttachment(f.t.Context(), messageID, "synthetic.bin", "application/octet-stream", path, hash, size))
	require.NoError(f.st.DB().QueryRowContext(f.t.Context(), f.st.Rebind("SELECT id FROM attachments WHERE message_id = ?"), messageID).Scan(&attachmentID))
	return
}
func (f *fixture) write(path string, data []byte) string {
	f.t.Helper()
	require := require.New(f.t)
	full := filepath.Join(f.root, filepath.FromSlash(path))
	require.NoError(os.MkdirAll(filepath.Dir(full), 0o700))
	require.NoError(os.WriteFile(full, data, 0o600))
	return full
}
func options() attachmentverify.Options {
	return attachmentverify.Options{Limit: 100, MaxBytes: 1 << 30, Timeout: time.Minute}
}
func (f *fixture) run() attachmentverify.Report {
	f.t.Helper()
	require := require.New(f.t)
	report, err := attachmentverify.Run(f.t.Context(), f.st, f.blobs, f.root, options())
	require.NoError(err)
	return report
}
func (f *fixture) packed(data []byte) (hash, path string) {
	f.t.Helper()
	require := require.New(f.t)
	layout, err := packstore.NewLayout(f.root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	require.NoError(os.MkdirAll(layout.PacksDir(), 0o700))
	writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(data)
	require.NoError(err)
	id := writer.ID()
	path = layout.PackPath(id)
	_, err = writer.Seal(path)
	require.NoError(err)
	hash = pack.ComputeBlobID(data).String()
	f.row(hash, hash[:2]+"/"+hash, len(data))
	require.NoError(f.st.RecordPackedBlobs(f.t.Context(), store.PackRecord{PackID: id, EntryCount: 1, StoredBytes: int64(entry.StoredLen), CreatedAt: time.Now().UTC()}, []store.PackIndexEntry{{BlobHash: hash, PackID: id, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}}))
	return
}

func TestPhysicalRepresentations(t *testing.T) {
	for _, name := range []string{"loose", "legacy", "packed", "missing loose", "missing pack", "loose modified", "loose truncated", "packed modified", "packed truncated", "wrong size", "uppercase", "missing hash"} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)
			data := []byte("synthetic physical bytes for verification")
			hash := pack.ComputeBlobID(data).String()
			path := hash[:2] + "/" + hash
			want := attachmentverify.Verified
			if strings.Contains(name, "packed") || name == "missing pack" {
				_, path = f.packed(data)
			} else {
				if name == "legacy" {
					path = "legacy/" + hash
				}
				if name == "uppercase" {
					hash = strings.ToUpper(hash)
				}
				f.row(hash, path, len(data))
				path = f.write(path, data)
			}
			switch name {
			case "missing loose":
				require.NoError(os.Remove(path))
				want = attachmentverify.MissingLoose
			case "missing pack":
				require.NoError(os.Remove(path))
				want = attachmentverify.PackUnavailable
			case "loose modified":
				require.NoError(os.WriteFile(path, []byte(strings.Repeat("X", len(data))), 0o600))
				want = attachmentverify.Corrupt
			case "loose truncated":
				require.NoError(os.Truncate(path, 3))
				want = attachmentverify.Corrupt
			case "packed truncated":
				require.NoError(os.Truncate(path, 10))
				want = attachmentverify.Corrupt
			case "packed modified":
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				require.NoError(err)
				_, err = file.WriteAt([]byte("BROKEN"), 10)
				require.NoError(err)
				require.NoError(file.Close())
				want = attachmentverify.Corrupt
			case "wrong size":
				_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind("UPDATE attachments SET size = ?"), len(data)+1)
				require.NoError(err)
				want = attachmentverify.MetadataMismatch
			case "missing hash":
				_, err := f.st.DB().ExecContext(t.Context(), "UPDATE attachments SET content_hash = ''")
				require.NoError(err)
				want = attachmentverify.Inconclusive
			}
			report := f.run()
			require.Len(report.References, 1)
			assert.Equal(want, report.References[0].Status)
			if want == attachmentverify.Verified {
				assert.Equal(1, report.ContentsVerified)
				assert.Equal(int64(len(data)), report.BytesRead)
			} else {
				assert.Zero(report.ReferencesVerified)
			}
			if name == "wrong size" {
				assert.Equal(1, report.ContentsVerified, "bytes still have verified identity")
			}
		})
	}
}

func TestSharedContentThumbnailsAndRetainedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	data := []byte("shared bytes")
	hash := pack.ComputeBlobID(data).String()
	path := hash[:2] + "/" + hash
	id, msg := f.row(hash, path, len(data))
	f.row(strings.ToUpper(hash), path, len(data))
	f.write(path, data)
	_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind("UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ? WHERE id = ?"), hash, path, id)
	require.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP, deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), msg)
	require.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind("INSERT INTO source_merge_archive_only_messages (message_id) VALUES (?)"), msg)
	require.NoError(err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind("INSERT INTO source_merge_preserved_attachments (attachment_id) VALUES (?)"), id)
	require.NoError(err)
	// One connection proves selection has released its cursor before resolution.
	f.st.DB().SetMaxOpenConns(1)
	report := f.run()
	assert.Equal(2, report.RowsSelected)
	assert.Equal(3, report.ReferencesSelected)
	assert.Equal(1, report.DistinctContents)
	assert.Equal(1, report.ContentsOpened)
	assert.Equal(3, report.ReferencesVerified)
	assert.Equal(int64(len(data)), report.BytesRead)
	require.NoError(os.Remove(filepath.Join(f.root, path)))
	report = f.run()
	assert.Equal(3, report.ReferencesAffected)
	assert.Equal(3, report.Counts[attachmentverify.MissingLoose])
}

func TestOmissionsAndUnverifiableReferences(t *testing.T) {
	for _, tc := range []struct{ name, hash, path, state, reason, want string }{
		{"policy", "", "", "skipped", "policy_scope", attachmentverify.Omitted},
		{"fetch failure", "", "", "failed", "fetch_failure", attachmentverify.NotMaterialized},
		{"unavailable", "", "", "unavailable", "source_unavailable", attachmentverify.NotMaterialized},
		{"unexplained skip", "", "", "skipped", "", attachmentverify.NotMaterialized},
		{"external", "", "https://files.example.com/content", "pending", "", attachmentverify.NotMaterialized},
		{"invalid hash", "invalid", "legacy/file", "stored", "", attachmentverify.Inconclusive},
		{"escaping path", strings.Repeat("a", 64), "../outside", "stored", "", attachmentverify.Inconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)
			id, _ := f.row(tc.hash, tc.path, 10)
			_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind("UPDATE attachments SET attachment_state = ?, attachment_skip_reason = ? WHERE id = ?"), tc.state, tc.reason, id)
			require.NoError(err)
			report := f.run()
			assert.Equal(tc.want, report.References[0].Status)
			assert.Zero(report.ReferencesVerified)
			assert.Zero(report.ReferencesAffected)
		})
	}
}

func TestSelectionAndByteBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	for _, data := range [][]byte{[]byte("first content"), []byte("second content")} {
		hash := pack.ComputeBlobID(data).String()
		path := hash[:2] + "/" + hash
		f.row(hash, path, len(data))
		f.write(path, data)
	}
	opts := options()
	opts.Limit = 1
	first, err := attachmentverify.Run(t.Context(), f.st, f.blobs, f.root, opts)
	require.NoError(err)
	assert.True(first.More)
	assert.Equal("partial", first.State)
	assert.Equal(1, first.ReferencesVerified)
	opts.AfterID = first.NextAfterID
	second, err := attachmentverify.Run(t.Context(), f.st, f.blobs, f.root, opts)
	require.NoError(err)
	assert.False(second.More)
	assert.Equal("complete", second.State)
	assert.NotEqual(first.References[0].AttachmentID, second.References[0].AttachmentID)
	opts = options()
	opts.MaxBytes = 3
	bounded, err := attachmentverify.Run(t.Context(), f.st, f.blobs, f.root, opts)
	require.NoError(err)
	assert.Equal("partial", bounded.State)
	assert.Equal("byte limit", bounded.StopReason)
	assert.Equal(int64(4), bounded.BytesRead)
	assert.Zero(bounded.ReferencesVerified)
	assert.Equal(int64(0), bounded.NextAfterID, "resumption must not skip unfinished rows")
	assert.Equal(2, bounded.Counts[attachmentverify.NotChecked])
}

// These wrappers retain the real physical stream. Fault injection is only at
// the consumption boundary, where OS close/read failures are not portable to
// reproduce; the primary open, hash and pack validation are never stubbed.
type boundaryStore struct {
	real   attachmentverify.StreamStore
	wrap   func(io.ReadCloser) io.ReadCloser
	opened func()
}

func (s boundaryStore) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	r, n, err := s.real.OpenStream(ctx, hash)
	if err != nil {
		return r, n, err
	}
	if s.opened != nil {
		s.opened()
	}
	if s.wrap != nil {
		r = s.wrap(r)
	}
	return r, n, nil
}

type closeFault struct{ io.ReadCloser }

func (r closeFault) Close() error {
	return errors.Join(r.ReadCloser.Close(), errors.New("injected close failure"))
}

type lateFault struct{ io.ReadCloser }

func (r lateFault) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

type cancelRead struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (r cancelRead) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p[:min(3, len(p))])
	r.cancel()
	return n, err
}

func TestLateErrorsCancellationAndConcurrentDeletion(t *testing.T) {
	for _, name := range []string{"close", "late read", "cancel", "delete", "permission"} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newFixture(t)
			data := []byte("physical stream with delayed boundary errors")
			hash := pack.ComputeBlobID(data).String()
			path := hash[:2] + "/" + hash
			id, _ := f.row(hash, path, len(data))
			full := f.write(path, data)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := boundaryStore{real: f.blobs}
			switch name {
			case "close":
				reader.wrap = func(r io.ReadCloser) io.ReadCloser { return closeFault{r} }
			case "late read":
				reader.wrap = func(r io.ReadCloser) io.ReadCloser { return lateFault{r} }
			case "cancel":
				reader.wrap = func(r io.ReadCloser) io.ReadCloser { return cancelRead{ReadCloser: r, cancel: cancel} }
			case "delete":
				reader.opened = func() {
					_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind("DELETE FROM attachments WHERE id = ?"), id)
					require.NoError(err)
				}
			case "permission":
				require.NoError(os.Chmod(full, 0))
				t.Cleanup(func() { require.NoError(os.Chmod(full, 0o600)) })
				probe, err := os.Open(full)
				if err == nil {
					require.NoError(probe.Close())
					t.Skip("current user can read files with mode 000")
				}
				require.ErrorIs(err, os.ErrPermission)
			}
			report, err := attachmentverify.Run(ctx, f.st, reader, f.root, options())
			require.NoError(err)
			assert.Zero(report.ContentsVerified)
			assert.Zero(report.ReferencesVerified)
			assert.Zero(report.ReferencesAffected)
			if name == "cancel" {
				assert.Equal("partial", report.State)
				assert.Equal(attachmentverify.NotChecked, report.References[0].Status)
			} else {
				assert.Equal(attachmentverify.Inconclusive, report.References[0].Status)
			}
		})
	}
}

func TestMissingPackDoesNotUseLooseCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	data := []byte("valid loose copy cannot hide absent pack authority")
	hash, path := f.packed(data)
	f.write(hash[:2]+"/"+hash, data)
	require.NoError(os.Remove(path))
	report := f.run()
	assert.Equal(attachmentverify.PackUnavailable, report.References[0].Status)
	assert.Zero(report.ContentsVerified)
	var count int
	require.NoError(f.st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM attachment_pack_index").Scan(&count))
	assert.Equal(1, count, "read-only diagnostic preserves authority")
}

func TestConcurrentPackingAndNewReference(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	data := []byte("loose content moves while the real stream is open")
	hash := pack.ComputeBlobID(data).String()
	path := hash[:2] + "/" + hash
	f.row(hash, path, len(data))
	f.write(path, data)
	layout, err := packstore.NewLayout(f.root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	maintainer, err := packstore.NewMaintainer(store.NewPackCatalog(f.st), layout, packstore.MaintainerOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintainer.Close()) })
	physical := attachmentstore.Wrap(maintainer.Store())
	reader := boundaryStore{real: physical, opened: func() {
		stats, err := maintainer.Pack(t.Context(), packstore.PackOptions{})
		require.NoError(err)
		assert.Equal(1, stats.BlobsPacked)
		// Created after selection: this reference cannot be certified by this run.
		other := []byte("new after selection")
		otherHash := pack.ComputeBlobID(other).String()
		otherPath := otherHash[:2] + "/" + otherHash
		f.row(otherHash, otherPath, len(other))
		f.write(otherPath, other)
	}}
	report, err := attachmentverify.Run(t.Context(), f.st, reader, f.root, options())
	require.NoError(err)
	assert.Equal(1, report.RowsSelected)
	assert.Equal(attachmentverify.Inconclusive, report.References[0].Status)
	assert.Zero(report.ReferencesVerified)
	assert.Zero(report.ReferencesAffected)
	assert.Equal("complete", f.run().State, "a fresh selection sees the new reference and pack authority")
}

func TestSQLFailureAndPreCancelledSelection(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := attachmentverify.Run(ctx, f.st, f.blobs, f.root, options())
	require.ErrorIs(err, context.Canceled)
	require.NoError(f.st.Close())
	_, err = attachmentverify.Run(t.Context(), f.st, f.blobs, f.root, options())
	require.Error(err)
}

func TestThumbnailOnlyAndUnknownSize(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	data := []byte("thumbnail with no SQL size")
	hash := pack.ComputeBlobID(data).String()
	path := hash[:2] + "/" + hash
	id, _ := f.row("", "https://files.example.com/unretained", 0)
	f.write(path, data)
	_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind("UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ? WHERE id = ?"), hash, path, id)
	require.NoError(err)
	report := f.run()
	require.Len(report.References, 2)
	assert.Equal(attachmentverify.NotMaterialized, report.References[0].Status)
	assert.Equal(attachmentverify.Verified, report.References[1].Status)
	assert.Nil(report.References[1].ExpectedSize)
	assert.Equal(1, report.ContentsVerified)
	other, _ := f.row(hash, path, len(data))
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind("UPDATE attachments SET size = NULL WHERE id = ?"), other)
	require.NoError(err)
	report = f.run()
	assert.Equal(2, report.ReferencesVerified)
	assert.Equal(1, report.DistinctContents)
}

func TestUnavailableCatalogueDoesNotBypassAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	data := []byte("loose bytes without a resolvable catalogue")
	hash := pack.ComputeBlobID(data).String()
	path := hash[:2] + "/" + hash
	f.row(hash, path, len(data))
	f.write(path, data)
	_, err := f.st.DB().ExecContext(t.Context(), "DROP TABLE attachment_pack_index")
	require.NoError(err)
	report := f.run()
	assert.Equal(attachmentverify.Inconclusive, report.References[0].Status)
	assert.Zero(report.ContentsOpened)
	assert.Zero(report.ContentsVerified)
}

func TestDiagnosticWorksWithReadOnlySQL(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFixture(t)
	data := []byte("bytes must remain unchanged")
	hash := pack.ComputeBlobID(data).String()
	path := hash[:2] + "/" + hash
	f.row(hash, path, len(data))
	full := f.write(path, data)
	f.st.DB().SetMaxOpenConns(1)
	query := "PRAGMA query_only = ON"
	if f.st.IsPostgreSQL() {
		query = "SET default_transaction_read_only = on"
	}
	_, err := f.st.DB().ExecContext(t.Context(), query)
	require.NoError(err)
	report := f.run()
	assert.Equal(1, report.ReferencesVerified)
	after, err := os.ReadFile(full)
	require.NoError(err)
	assert.Equal(data, after)
}
