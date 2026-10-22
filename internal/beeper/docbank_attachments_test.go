package beeper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/docbanktest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// mirrorRig is a Docbank test double plus an archive holding stored attachments.
type mirrorRig struct {
	*docbanktest.UploadHandler

	t      *testing.T
	f      *storetest.Fixture
	dir    string
	worker *AttachmentMirror
	// Faults: an HTTP status, or one of the fault constants for uploads.
	folderStatus atomic.Int32
	count        int
}

func newMirrorRig(t *testing.T) *mirrorRig {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	r := &mirrorRig{t: t, f: storetest.New(t), dir: t.TempDir(), UploadHandler: docbanktest.NewUploadHandler(t)}
	blobs, err := attachmentstore.New(store.NewPackCatalog(r.f.Store), r.dir)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(blobs.Close()) })
	srv := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)
	client, err := docbankmedia.NewClient(srv.URL, nil)
	require.NoError(err)
	r.worker = NewAttachmentMirror(r.f.Store, blobs, client, "dest", AttachmentMirrorOptions{URL: srv.URL, Folder: "/msgvault", AttachmentsDir: r.dir})
	return r
}

func (r *mirrorRig) serve(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/api/v1/path":
		if code := int(r.folderStatus.Load()); code != 0 {
			if code == docbanktest.FaultInvalidReceipt {
				_, _ = io.WriteString(w, `{"id":7,"kind":"file"}`)
			} else {
				w.WriteHeader(code)
			}
			return
		}
		_, _ = io.WriteString(w, `{"id":7,"kind":"dir"}`)
		return
	}
	r.ServeHTTP(w, req)
}

// add stores one attachment and returns its blob hash.
func (r *mirrorRig) add(name, mimeType string, data []byte) string {
	r.t.Helper()
	r.count++
	return addStoredMediaSource(r.t, &mediaWorld{st: r.f.Store, dir: r.dir}, "gmail", "test@example.com", fmt.Sprintf("message-%d", r.count), data,
		name, mimeType, "", store.AttachmentRoleStandalone, "stored", nil, "part", "part")
}

// runBatch runs one batch, asserts it left no work and returns its error.
func (r *mirrorRig) runBatch(ctx context.Context) error {
	r.t.Helper()
	more, err := r.worker.RunBatch(ctx)
	assert.False(r.t, more, "batch left work")
	return err
}

func (r *mirrorRig) blobPath(hash string) string { return filepath.Join(r.dir, hash[:2], hash) }

func (r *mirrorRig) status() store.DocbankAttachmentSummary {
	r.t.Helper()
	require := require.New(r.t)
	status, err := r.f.Store.DocbankAttachmentStatus(r.t.Context(), "dest")
	require.NoError(err)
	return status
}

func (r *mirrorRig) makeRetriesDue() {
	r.t.Helper()
	_, err := r.f.Store.DB().Exec(`UPDATE docbank_attachment_deliveries SET next_action_at=NULL WHERE state='sending'`)
	require.NoError(r.t, err)
}

func TestAttachmentMirrorUploadsEachFileOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	r.add("report.pdf", "application/pdf", []byte("synthetic document"))
	r.add("alias.pdf", "application/pdf", []byte("synthetic document"))
	require.NoError(r.runBatch(t.Context()))
	require.NoError(r.runBatch(t.Context()))
	assert.Equal(int32(1), r.Uploads.Load())
}

func TestAttachmentMirrorDeletedBetweenDiscoveryAndIntent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	r.add("deleted.pdf", "application/pdf", []byte("deleted document"))
	// Folder resolution separates discovery from saving the upload intent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/v1/path" {
			_, err := r.f.Store.DB().Exec(`DELETE FROM attachments`)
			assert.NoError(err)
		}
		r.serve(w, req)
	}))
	t.Cleanup(srv.Close)
	client, err := docbankmedia.NewClient(srv.URL, nil)
	require.NoError(err)
	r.worker.client = client
	require.NoError(r.runBatch(t.Context()))
	assert.Zero(r.Uploads.Load())
}

func TestAttachmentMirrorUploadNamesAndInvalidUTF8(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	want := map[string][2]string{}
	for i, tc := range []struct{ name, mimeType, wantName, wantType string }{
		{"report.pdf", "application/pdf", "report.pdf", "application/pdf"},
		{strings.Repeat("界", 300) + ".pdf", "application/pdf", strings.Repeat("界", 300) + ".pdf", "application/pdf"},
		{"a/b\\c?.pdf", "application/pdf", "c?.pdf", "application/pdf"},
		{"report\r\n\t\x7f.pdf", "application/pdf", "report____.pdf", "application/pdf"},
		{"", "application/pdf", "attachment", "application/pdf"},
		{".", "application/pdf", "attachment", "application/pdf"},
		{"..", "application/pdf", "attachment", "application/pdf"},
		{"broken.bin", "broken mime", "broken.bin", "application/octet-stream"},
	} {
		hash := r.add(tc.name, tc.mimeType, fmt.Appendf(nil, "document %d", i))
		want[hash] = [2]string{hash[:12] + "-" + tc.wantName, tc.wantType}
	}
	require.NoError(r.runBatch(t.Context()))
	assert.Equal(int64(len(want)), r.status().Delivered)
	for hash, wantParts := range want {
		got, _ := r.Uploaded.Load(hash)
		assert.Equal(wantParts, got)
	}
	// Invalid UTF-8 cannot be stored in PostgreSQL text, so exercise the upload step directly.
	hash := r.add("placeholder", "application/pdf", []byte("invalid name"))
	packed, err := attachmentstore.New(store.NewDocbankAttachmentPackResolver(r.f.Store), r.dir)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(packed.Close()) })
	_, err = r.worker.upload(t.Context(), 7, attachmentUploadMetadata(store.DocbankAttachmentDelivery{ContentHash: hash, Name: "\xff"}), packed)
	require.NoError(err)
	got, _ := r.Uploaded.Load(hash)
	assert.Equal([2]string{hash[:12] + "-attachment", "application/octet-stream"}, got)
}

func TestAttachmentMirrorDestinationFaults(t *testing.T) {
	for _, tc := range []struct {
		name         string
		folderStatus int
		uploadStatus int
		reason       string
		large        bool
	}{
		{"folder is a file", docbanktest.FaultInvalidReceipt, 0, "", false},
		{"lookup 400", http.StatusBadRequest, 0, "", false},
		{"unauthorized upload", 0, http.StatusUnauthorized, "unauthorized", false},
		{"forbidden upload", 0, http.StatusForbidden, "forbidden", false},
		{"missing upload", 0, http.StatusNotFound, "not_found", false},
		{"conflict", 0, http.StatusConflict, "conflict", false},
		{"timed out upload", 0, http.StatusRequestTimeout, "timeout", false},
		{"large rate limited upload", 0, http.StatusTooManyRequests, "rate_limited", true},
		{"large server unavailable", 0, http.StatusServiceUnavailable, "server_error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			expectedFiles := int64(1)
			if tc.uploadStatus == http.StatusConflict {
				r.add("first.pdf", "application/pdf", []byte("preceding document"))
				expectedFiles++
			}
			data := []byte("synthetic document")
			if tc.large {
				data = make([]byte, 32<<20)
			}
			hash := r.add("report.pdf", "application/pdf", data)
			if tc.large || tc.uploadStatus == http.StatusConflict {
				r.add("later.pdf", "application/pdf", []byte("later document"))
				expectedFiles++
			}
			r.folderStatus.Store(int32(tc.folderStatus))
			r.UploadFaults.Store(hash, tc.uploadStatus)
			uploadErr := r.runBatch(t.Context())
			if tc.uploadStatus == http.StatusConflict {
				require.NoError(uploadErr)
			} else {
				require.Error(uploadErr)
			}
			require.NotErrorIs(uploadErr, errAttachmentSourceIO)
			if expectedFiles > 1 {
				assert.Equal(int32(expectedFiles), r.Uploads.Load())
				assert.Equal(expectedFiles-1, r.status().Delivered)
			}
			status := r.status()
			assert.Zero(status.Failed)
			assert.Zero(status.Remaining, "discovery queues files during a Docbank outage")
			assert.Equal(int64(1), status.Sending)
			intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
			require.NoError(err)
			if r.folderStatus.Load() != 0 {
				require.Len(intents, 1, "a folder failure leaves the queued intent due")
				assert.Zero(intents[0].Attempts)
				assert.Empty(status.Reasons)
			} else {
				assert.Empty(intents)
				assert.Equal(map[string]int64{tc.reason: 1}, status.Reasons)
			}
			scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
			require.NoError(err)
			assert.Equal(expectedFiles, scan.AfterAttachmentID)
			r.folderStatus.Store(0)
			r.UploadFaults.Clear()
			r.makeRetriesDue()
			require.NoError(r.runBatch(t.Context()))
			assert.Equal(expectedFiles, r.status().Delivered)
			assert.Empty(r.status().Reasons)
		})
	}
}

func TestAttachmentMirrorFileFailuresAndBackfill(t *testing.T) {
	for _, fault := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, docbanktest.FaultInvalidReceipt} {
		t.Run(strconv.Itoa(fault), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			bad := r.add("bad.pdf", "application/pdf", []byte("first document"))
			r.add("good.pdf", "application/pdf", []byte("second document"))
			r.UploadFaults.Store(bad, fault)
			require.NoError(r.runBatch(t.Context()))
			assert.Equal(int64(1), r.status().Failed)
			assert.Equal(int64(1), r.status().Delivered)
			original, accepted := r.Nodes.Load([3]string{"7", bad[:12] + "-bad.pdf", bad})
			assert.Equal(fault == docbanktest.FaultInvalidReceipt, accepted)
			r.UploadFaults.Clear()
			require.NoError(r.f.Store.RetryDocbankAttachments(t.Context(), "dest"))
			require.NoError(r.runBatch(t.Context()))
			if accepted {
				refs, err := store.LoadDocbankAttachmentRefs(t.Context(), r.f.Store.DB(), r.f.Store.Rebind, "", []string{bad})
				require.NoError(err)
				require.Len(refs[bad], 1)
				assert.Equal(original, refs[bad][0].NodeID)
			}
			count := 0
			r.Nodes.Range(func(_, _ any) bool { count++; return true })
			assert.Equal(2, count)
			assert.Equal(int32(3), r.Uploads.Load())
		})
	}
}

func TestAttachmentMirrorDestinationFaultStreak(t *testing.T) {
	for _, fault := range []int{0, http.StatusBadRequest, http.StatusConflict} {
		t.Run(strconv.Itoa(fault), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			for i := range 4 {
				hash := r.add("report.pdf", "application/pdf", fmt.Appendf(nil, "document %d", i))
				switch i {
				case 0, 2:
					r.UploadFaults.Store(hash, http.StatusServiceUnavailable)
				case 1:
					r.UploadFaults.Store(hash, fault)
				}
			}
			require.Error(r.runBatch(t.Context()))
			assert.Equal(int32(4), r.Uploads.Load())
			assert.Equal(int64(1)+map[bool]int64{true: 1}[fault == 0], r.status().Delivered)
		})
	}
}

func TestAttachmentMirrorSourceFaults(t *testing.T) {
	t.Run("loose sources", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		r := newMirrorRig(t)
		missing := r.add("missing.pdf", "application/pdf", []byte("missing document"))
		corrupt := r.add("corrupt.pdf", "application/pdf", []byte("corrupt document"))
		alias := r.add("alias.pdf", "application/pdf", []byte("unreferenced alias file"))
		_, err := r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET content_hash='',source_attachment_id=? WHERE content_hash=?`), "slack:alias", alias)
		require.NoError(err)
		invalid := r.add("invalid.pdf", "application/pdf", []byte("invalid hash document"))
		_, err = r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET content_hash=? WHERE content_hash=?`), strings.Repeat("z", 64), invalid)
		require.NoError(err)
		r.add("valid.pdf", "application/pdf", []byte("valid document"))
		require.NoError(os.Remove(r.blobPath(missing)))
		require.NoError(os.WriteFile(r.blobPath(corrupt), []byte("corrupt documenX"), 0600))
		require.NoError(r.runBatch(t.Context()))
		status := r.status()
		assert.Equal(int64(2), status.Delivered, "a bad source file does not stop the next file")
		_, delivered := r.Uploaded.Load(alias)
		assert.True(delivered)
		assert.Equal(int64(3), status.Reasons["source_unavailable"])
		intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
		require.NoError(err)
		assert.Empty(intents, "source failures wait for backfill")
		require.NoError(os.WriteFile(r.blobPath(missing), []byte("missing document"), 0600))
		_, err = r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET filename='restored.pdf' WHERE content_hash=?`), missing)
		require.NoError(err)
		require.NoError(r.runBatch(t.Context()))
		assert.Equal(int64(3), r.status().Delivered)
		assert.Equal(int64(2), r.status().Reasons["source_unavailable"])
	})
	t.Run("legacy loose path backfill", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		r := newMirrorRig(t)
		data := []byte("legacy document")
		hash := r.add("legacy.pdf", "application/pdf", data)
		storagePath := "synctech-sms/" + hash[:2] + "/" + hash
		file := filepath.Join(r.dir, filepath.FromSlash(storagePath))
		require.NoError(os.MkdirAll(filepath.Dir(file), 0700))
		require.NoError(os.Rename(r.blobPath(hash), file))
		_, err := r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET storage_path=? WHERE content_hash=?`), storagePath, hash)
		require.NoError(err)
		require.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), r.f.CreateMessage("missing legacy candidate"), store.AttachmentWrite{ContentHash: hash, StoragePath: "missing/" + hash, State: "stored"}))
		require.NoError(os.WriteFile(r.blobPath(hash), []byte("legacy documenX"), 0600))
		require.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), r.f.CreateMessage("canonical duplicate candidate"), store.AttachmentWrite{ContentHash: hash, StoragePath: hash[:2] + "/" + hash, State: "stored"}))
		require.NoError(os.WriteFile(file, []byte("legacy documenX"), 0600))
		require.NoError(r.runBatch(t.Context()))
		assert.Equal(int64(1), r.status().Reasons["source_unavailable"])
		assert.Zero(r.status().Delivered)
		require.NoError(os.WriteFile(file, data, 0600))
		require.NoError(r.f.Store.RetryDocbankAttachments(t.Context(), "dest"))
		require.NoError(r.runBatch(t.Context()))
		assert.Equal(int64(1), r.status().Delivered)
		assert.Zero(r.status().Failed)
		_, ok := r.Uploaded.Load(hash)
		require.True(ok)
	})

	for _, healthyCopy := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt pack/healthy recorded copy=%t", healthyCopy), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			data := []byte("packed document")
			hash := r.add("packed.pdf", "application/pdf", data)
			layout, err := packstore.NewLayout(r.dir, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
			require.NoError(err)
			require.NoError(pack.MkdirAllSynced(layout.PacksDir()))
			writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
			require.NoError(err)
			entry, err := writer.Append(data)
			require.NoError(err)
			packID := writer.ID()
			_, err = writer.Seal(layout.PackPath(packID))
			require.NoError(err)
			require.NoError(r.f.Store.RecordPackedBlobs(store.PackRecord{PackID: packID, EntryCount: 1, StoredBytes: int64(entry.StoredLen), CreatedAt: time.Now().UTC()}, []store.PackIndexEntry{{BlobHash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}}))
			require.NoError(os.Remove(r.blobPath(hash)))
			if healthyCopy {
				storagePath := "legacy/" + hash
				require.NoError(os.MkdirAll(filepath.Join(r.dir, "legacy"), 0700))
				require.NoError(os.WriteFile(filepath.Join(r.dir, filepath.FromSlash(storagePath)), data, 0600))
				_, err = r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET storage_path=? WHERE content_hash=?`), storagePath, hash)
				require.NoError(err)
			}
			file, err := os.OpenFile(layout.PackPath(packID), os.O_RDWR, 0)
			require.NoError(err)
			var damaged [1]byte
			_, err = file.ReadAt(damaged[:], int64(entry.Offset))
			require.NoError(err)
			damaged[0] ^= 0xff
			_, err = file.WriteAt(damaged[:], int64(entry.Offset))
			require.NoError(err)
			require.NoError(file.Close())
			require.NoError(r.runBatch(t.Context()))
			status := r.status()
			assert.Zero(status.Sending)
			assert.Zero(status.Remaining)
			if healthyCopy {
				assert.Equal(int64(1), status.Delivered)
				assert.Zero(status.Failed)
				_, delivered := r.Uploaded.Load(hash)
				assert.True(delivered)
			} else {
				assert.Zero(status.Delivered)
				assert.Equal(int64(1), status.Failed)
				assert.Equal(int64(1), status.Reasons["source_unavailable"])
			}
		})
	}
}

type faultAttachmentStore struct {
	*attachmentstore.Store

	stage string
	err   error
	hash  string
}

func (s faultAttachmentStore) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	if s.hash != "" && hash != s.hash {
		return s.Store.OpenStream(ctx, hash)
	}
	if s.stage == "open" {
		return nil, 0, s.err
	}
	r, size, err := s.Store.OpenStream(ctx, hash)
	if err != nil {
		return nil, size, err
	}
	return faultAttachmentReader{ReadCloser: r, stage: s.stage, err: s.err}, size, nil
}

type faultAttachmentReader struct {
	io.ReadCloser

	stage string
	err   error
}

func (r faultAttachmentReader) Read(p []byte) (int, error) {
	if r.stage == "read" {
		return 0, r.err
	}
	return r.ReadCloser.Read(p)
}

func (r faultAttachmentReader) Close() error {
	err := r.ReadCloser.Close()
	if r.stage == "close" {
		return errors.Join(err, r.err)
	}
	return err
}

func TestAttachmentMirrorSourceIOErrors(t *testing.T) {
	for _, tc := range []struct {
		stage string
		err   error
	}{
		{"open", errors.New("temporary open failure")},
		{"read", errors.New("temporary read failure")},
		{"close", errors.New("temporary close failure")},
	} {
		t.Run(tc.stage+"/"+tc.err.Error(), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			r.add("first.pdf", "application/pdf", []byte("first document"))
			hash := r.add("report.pdf", "application/pdf", []byte("source I/O failure"))
			r.add("third.pdf", "application/pdf", []byte("third document"))
			blobs, ok := r.worker.blobs.(*attachmentstore.Store)
			require.True(ok)
			r.worker.blobs = faultAttachmentStore{Store: blobs, stage: tc.stage, err: tc.err, hash: hash}
			err := r.runBatch(t.Context())
			require.NoError(err)
			assert.Equal(int64(2), r.status().Delivered)
			assert.Equal(int64(1), r.status().Sending)
			assert.Zero(r.status().Failed)
			intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
			require.NoError(err)
			assert.Empty(intents)
			assert.Equal(int64(1), r.status().Reasons["transport"])
			r.worker.blobs = blobs
			r.makeRetriesDue()
			require.NoError(r.runBatch(t.Context()))
			assert.Equal(int64(3), r.status().Delivered)
		})
	}
}

func TestAttachmentMirrorLostResponse(t *testing.T) {
	for _, fault := range []int{docbanktest.FaultDrop, 0} {
		t.Run(map[int]string{docbanktest.FaultDrop: "dropped response", 0: "cancelled receipt write"}[fault], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			hash := r.add("report.pdf", "application/pdf", []byte("lost response"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fault == docbanktest.FaultDrop {
				r.UploadFaults.Store(hash, docbanktest.FaultDrop)
			} else {
				r.worker.WithOperationGate(func(context.Context) (func(), bool) {
					if r.Uploads.Load() == 1 {
						cancel()
					}
					return func() {}, true
				})
			}
			err := r.runBatch(ctx)
			if fault == docbanktest.FaultDrop {
				require.Error(err)
			} else {
				require.ErrorIs(err, context.Canceled)
			}
			assert.Equal(int32(1), r.Uploads.Load())
			assert.Equal(int64(1), r.status().Sending)
			assert.Zero(r.status().Delivered)
			assert.Zero(r.status().Failed)
			assert.Zero(r.status().Remaining)
			r.UploadFaults.Clear()
			r.worker.WithOperationGate(nil)
			r.makeRetriesDue()
			_, err = r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET filename=?,mime_type=? WHERE content_hash=?`), "renamed.pdf", "text/plain", hash)
			require.NoError(err)
			require.NoError(r.runBatch(t.Context()))
			assert.Equal(int32(2), r.Uploads.Load())
			assert.Equal(int64(1), r.status().Delivered)
			refs, err := store.LoadDocbankAttachmentRefs(t.Context(), r.f.Store.DB(), r.f.Store.Rebind, "", []string{hash})
			require.NoError(err)
			require.Len(refs[hash], 1)
			node, ok := r.Nodes.Load([3]string{"7", hash[:12] + "-report.pdf", hash})
			require.True(ok)
			assert.Equal(node, refs[hash][0].NodeID)
			count := 0
			r.Nodes.Range(func(_, _ any) bool { count++; return true })
			assert.Equal(1, count)
			assert.Equal("version", refs[hash][0].VersionID)
			uploaded, _ := r.Uploaded.Load(hash)
			assert.Equal([2]string{hash[:12] + "-report.pdf", "application/pdf"}, uploaded)
		})
	}
}

func TestAttachmentMirrorOutageRecovery(t *testing.T) {
	for _, fault := range []int{http.StatusServiceUnavailable, docbanktest.FaultTimeout} {
		t.Run(strconv.Itoa(fault), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			if fault == docbanktest.FaultTimeout {
				r.worker.uploadTimeout = 100 * time.Millisecond
			}
			for i := range 3 {
				hash := r.add("report.pdf", "application/pdf", fmt.Appendf(nil, "document %d", i))
				r.UploadFaults.Store(hash, fault)
			}
			require.Error(r.runBatch(t.Context()))
			assert.Equal(int32(2), r.Uploads.Load())
			assert.Zero(r.status().Remaining, "discovery queued the file the outage left unsent")
			assert.Equal(int64(3), r.status().Sending)
			assert.Equal(map[string]int64{map[int]string{http.StatusServiceUnavailable: "server_error", docbanktest.FaultTimeout: "transport"}[fault]: 2}, r.status().Reasons)
			var retries int
			require.NoError(r.f.Store.DB().QueryRow(`SELECT SUM(attempts) FROM docbank_attachment_deliveries`).Scan(&retries))
			assert.Equal(2, retries)
			for range 2 {
				r.makeRetriesDue()
				require.Error(r.runBatch(t.Context()))
			}
			r.makeRetriesDue()
			intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
			require.NoError(err)
			require.Len(intents, 3)
			for _, intent := range intents {
				assert.Equal(2, intent.Attempts)
			}
			fresh := r.add("fresh.pdf", "application/pdf", []byte("fresh document"))
			require.Error(r.runBatch(t.Context()))
			_, delivered := r.Uploaded.Load(fresh)
			assert.True(delivered)
		})
	}
}

func TestAttachmentMirrorPersistentRetriesWait(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	mid := r.f.CreateMessage("persistent retries")
	hashSQL := "printf('%064x',i)"
	if r.f.Store.IsPostgreSQL() {
		hashSQL = "lpad(to_hex(i),64,'0')"
	}
	_, err := r.f.Store.DB().Exec(r.f.Store.Rebind(`WITH RECURSIVE ids(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM ids WHERE i<500) INSERT INTO attachments(message_id,content_hash,storage_path,attachment_state) SELECT ?,`+hashSQL+`,SUBSTR(`+hashSQL+`,1,2)||'/'||`+hashSQL+`,'stored' FROM ids`), mid)
	require.NoError(err)
	_, err = r.f.Store.DB().Exec(`INSERT INTO docbank_attachment_deliveries(destination_key,content_hash,upload_name,mime_type,state,endpoint,folder) SELECT 'dest',content_hash,'file.pdf','application/pdf','sending','','' FROM attachments`)
	require.NoError(err)
	blobs, ok := r.worker.blobs.(*attachmentstore.Store)
	require.True(ok)
	r.worker.blobs = faultAttachmentStore{Store: blobs, stage: "open", err: os.ErrPermission}
	require.NoError(r.runBatch(t.Context()))
	assert.Equal(int64(500), r.status().Sending)
	assert.Equal(int64(500), r.status().Reasons["transport"])
	require.NoError(r.runBatch(t.Context()))
	var attempts int
	require.NoError(r.f.Store.DB().QueryRow(`SELECT SUM(attempts) FROM docbank_attachment_deliveries`).Scan(&attempts))
	assert.Equal(500, attempts)
	intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
	require.NoError(err)
	assert.Empty(intents)
}

func TestAttachmentMirrorPackedAliasAfterDonorReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	data := []byte("retained packed alias")
	hash := r.add("donor.pdf", "application/pdf", data)
	mid := r.f.CreateMessage("alias")
	require.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{StoragePath: hash[:2] + "/" + hash, SourceAttachmentID: "slack:alias", Filename: "alias.pdf", MIMEType: "application/pdf", State: "stored"}))
	layout, err := packstore.NewLayout(r.dir, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	require.NoError(pack.MkdirAllSynced(layout.PacksDir()))
	writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
	require.NoError(err)
	entry, err := writer.Append(data)
	require.NoError(err)
	packID := writer.ID()
	_, err = writer.Seal(layout.PackPath(packID))
	require.NoError(err)
	require.NoError(r.f.Store.RecordPackedBlobs(store.PackRecord{PackID: packID, EntryCount: 1, StoredBytes: int64(entry.StoredLen), CreatedAt: time.Now().UTC()}, []store.PackIndexEntry{{BlobHash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}}))
	require.NoError(os.Remove(r.blobPath(hash)))
	_, err = r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET content_hash=?,storage_path='' WHERE content_hash=?`), strings.Repeat("b", 64), hash)
	require.NoError(err)
	_, _, err = r.worker.blobs.OpenStream(t.Context(), hash)
	require.ErrorIs(err, os.ErrNotExist)
	require.NoError(r.runBatch(t.Context()))
	assert.Equal(int64(1), r.status().Delivered)
	assert.Zero(r.status().Failed)
	assert.Equal(int32(1), r.Uploads.Load())
}

func TestAttachmentMirrorPassProgress(t *testing.T) {
	t.Run("upload exhausts pass budget", func(t *testing.T) {
		must := require.New(t)
		assert := assert.New(t)
		r := newMirrorRig(t)
		retry := r.add("retry.pdf", "application/pdf", []byte("saved retry"))
		_, err := r.f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", attachmentUploadMetadata(store.DocbankAttachmentDelivery{ContentHash: retry, Name: "retry.pdf", MIMEType: "application/pdf"}))
		must.NoError(err)
		fresh := r.add("fresh.pdf", "application/pdf", []byte("fresh file"))
		later := r.add("later.pdf", "application/pdf", []byte("later file"))
		transport := http.DefaultTransport
		defer func() { http.DefaultTransport = transport }()
		synctest.Test(t, func(t *testing.T) {
			require := require.New(t)
			deadline := time.Now().Add(time.Second)
			http.DefaultTransport = handlerTransport(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("X-Docbank-Blob-Hash") == fresh {
					synctest.Sleep(time.Until(deadline))
				}
				r.serve(w, req)
			})
			more, err := r.worker.runBatch(t.Context(), deadline)
			require.NoError(err)
			require.True(more)
		})
		http.DefaultTransport = transport
		_, delivered := r.Uploaded.Load(retry)
		assert.False(delivered, "the saved retry, queued after new files, waits for another pass")
		scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
		must.NoError(err)
		var laterID int64
		must.NoError(r.f.Store.DB().QueryRow(r.f.Store.Rebind(`SELECT id FROM attachments WHERE content_hash=?`), later).Scan(&laterID))
		assert.Equal(laterID, scan.AfterAttachmentID, "discovery finishes before a slow upload")
		must.NoError(r.runBatch(t.Context()))
		assert.Equal(int64(3), r.status().Delivered)
		assert.Equal(int32(3), r.Uploads.Load())
	})

	t.Run("journal acknowledgement and outage replay", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		r := newMirrorRig(t)
		late := r.add("late.pdf", "application/pdf", []byte("late document"))
		_, err := r.f.Store.DB().Exec(`UPDATE attachments SET attachment_state='pending'`)
		require.NoError(err)
		var id int64
		require.NoError(r.f.Store.DB().QueryRow(`SELECT id FROM attachments`).Scan(&id))
		require.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), r.f.CreateMessage("later metadata"), store.AttachmentWrite{Filename: "metadata"}))
		require.NoError(r.runBatch(t.Context()))
		scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
		require.NoError(err)
		require.Greater(scan.AfterAttachmentID, id)
		assert.Zero(r.status().Remaining)
		assert.True(scan.NextFullScanAt.After(time.Now().Add(23 * time.Hour)))
		_, err = r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET attachment_state='stored' WHERE content_hash=?`), late)
		require.NoError(err)
		assert.Equal(int64(1), r.status().Remaining)
		assert.Zero(r.status().Sending)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.worker.WithOperationGate(func(context.Context) (func(), bool) {
			var queued int
			require.NoError(r.f.Store.DB().QueryRow(`SELECT COUNT(*) FROM docbank_attachment_deliveries WHERE state='sending'`).Scan(&queued))
			if queued > 0 {
				cancel()
				return nil, false
			}
			return func() {}, true
		})
		require.ErrorIs(r.runBatch(ctx), context.Canceled)
		intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
		require.NoError(err)
		require.Len(intents, 1)
		assert.Zero(intents[0].Attempts)
		changes, err := r.f.Store.ListAttachmentChanges(t.Context(), store.DocbankAttachmentConsumerKey("dest"), 500)
		require.NoError(err)
		assert.NotEmpty(changes, "persisted work survives an interrupted acknowledgement")
		r.worker.WithOperationGate(nil)
		r.folderStatus.Store(http.StatusServiceUnavailable)
		require.Error(r.runBatch(t.Context()))
		changes, err = r.f.Store.ListAttachmentChanges(t.Context(), store.DocbankAttachmentConsumerKey("dest"), 500)
		require.NoError(err)
		assert.Empty(changes, "journal replay progresses during a folder outage")
		r.folderStatus.Store(0)
		require.NoError(r.runBatch(t.Context()))
		assert.Zero(r.status().Remaining)
		assert.Equal(int64(1), r.status().Delivered)
		assert.Equal(int32(1), r.Uploads.Load())
		_, delivered := r.Uploaded.Load(late)
		assert.True(delivered)
	})

	for _, driver := range []string{"imports", "deadline"} {
		t.Run("saved retries and replay/"+driver, func(t *testing.T) {
			must := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			var fresh string
			if driver == "imports" {
				fresh = r.add("late.pdf", "application/pdf", []byte("late document"))
				_, err := r.f.Store.DB().Exec(`UPDATE attachments SET attachment_state='pending'`)
				must.NoError(err)
			}
			must.NoError(r.runBatch(t.Context()))
			if driver == "imports" {
				_, err := r.f.Store.DB().Exec(r.f.Store.Rebind(`UPDATE attachments SET attachment_state='stored' WHERE content_hash=?`), fresh)
				must.NoError(err)
			}
			retry := r.add("retry.pdf", "application/pdf", []byte("saved retry"))
			_, err := r.f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", attachmentUploadMetadata(store.DocbankAttachmentDelivery{ContentHash: retry, Name: "retry.pdf", MIMEType: "application/pdf"}))
			must.NoError(err)
			scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
			must.NoError(err)
			after := scan
			after.AfterAttachmentID++
			swapped, err := r.f.Store.AdvanceDocbankAttachmentScan(t.Context(), "dest", scan, after)
			must.NoError(err)
			must.True(swapped)
			if driver == "deadline" {
				fresh = r.add("fresh.pdf", "application/pdf", []byte("discovered file"))
			}
			mid := r.f.CreateMessage("journal backlog")
			for i := range 501 {
				must.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "metadata", SourcePartKey: fmt.Sprintf("backlog:%d", i)}))
			}
			imports := 0
			transport := http.DefaultTransport
			defer func() { http.DefaultTransport = transport }()
			synctest.Test(t, func(t *testing.T) {
				require := require.New(t)
				http.DefaultTransport = handlerTransport(r.serve)
				r.worker.WithOperationGate(func(context.Context) (func(), bool) {
					if driver == "imports" {
						imports++
						synctest.Sleep(2 * time.Second)
						r.add("incoming.pdf", "application/pdf", fmt.Appendf(nil, "incoming document %d", imports))
					} else {
						synctest.Sleep(10 * time.Millisecond)
					}
					return func() {}, true
				})
				if driver == "imports" {
					more, err := r.worker.RunBatch(t.Context())
					require.NoError(err)
					require.True(more)
				} else {
					more, err := r.worker.runPass(t.Context(), time.Now().Add(time.Second))
					require.NoError(err)
					require.True(more)
				}
			})
			for _, hash := range []string{fresh, retry} {
				_, delivered := r.Uploaded.Load(hash)
				assert.True(delivered)
			}
			if driver == "imports" {
				assert.Positive(imports)
				assert.LessOrEqual(imports, 30)
			}
		})
	}
}

func TestAttachmentMirrorReplayDeadline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	require.NoError(r.runBatch(t.Context()))
	first := r.add("first.pdf", "application/pdf", []byte("first replay file"))
	second := r.add("second.pdf", "application/pdf", []byte("second replay file"))
	consumer := store.DocbankAttachmentConsumerKey("dest")
	changes, err := r.f.Store.ListAttachmentChanges(t.Context(), consumer, 500)
	require.NoError(err)
	require.Len(changes, 2)
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Second)
		acquisitions := 0
		r.worker.WithOperationGate(func(context.Context) (func(), bool) {
			acquisitions++
			if acquisitions == 1 {
				synctest.Sleep(time.Until(deadline))
			}
			return func() {}, true
		})
		more, err := r.worker.replayAttachmentChanges(t.Context(), deadline)
		require.NoError(err)
		require.True(more)
		state, err := r.f.Store.GetAttachmentChangeConsumer(t.Context(), consumer)
		require.NoError(err)
		assert.Equal(changes[0].Sequence, state.LastSequence)
		remaining, err := r.f.Store.ListAttachmentChanges(t.Context(), consumer, 500)
		require.NoError(err)
		require.Len(remaining, 1)
		assert.Equal(changes[1].Sequence, remaining[0].Sequence)
		more, err = r.worker.replayAttachmentChanges(t.Context(), deadline)
		require.NoError(err)
		require.True(more)
		unchanged, err := r.f.Store.GetAttachmentChangeConsumer(t.Context(), consumer)
		require.NoError(err)
		assert.Equal(state, unchanged, "an expired pass acknowledges nothing")
	})
	r.worker.WithOperationGate(nil)
	require.NoError(r.runBatch(t.Context()))
	for _, hash := range []string{first, second} {
		_, delivered := r.Uploaded.Load(hash)
		assert.True(delivered)
	}
	remaining, err := r.f.Store.ListAttachmentChanges(t.Context(), consumer, 500)
	require.NoError(err)
	assert.Empty(remaining)
}

func TestAttachmentMirrorRestoredAfterDedup(t *testing.T) {
	for _, tc := range []struct {
		timing        string
		sourceDeleted bool
	}{
		{"after discovery cursor", false},
		{"after discovery cursor", true},
		{"between final page and completion", true},
	} {
		t.Run(tc.timing+"/source_deleted="+strconv.FormatBool(tc.sourceDeleted), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := newMirrorRig(t)
			hash := r.add("restored.pdf", "application/pdf", []byte("restored document"))
			if !tc.sourceDeleted {
				_, err := r.f.Store.SaveDocbankAttachmentIntent(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash, Name: "restored.pdf", MIMEType: "application/pdf"})
				require.NoError(err)
			}
			var hiddenID int64
			require.NoError(r.f.Store.DB().QueryRow(`SELECT message_id FROM attachments`).Scan(&hiddenID))
			if tc.sourceDeleted {
				_, err := r.f.Store.DB().Exec(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP`)
				require.NoError(err)
			}
			_, err := r.f.Store.MergeDuplicates(r.f.CreateMessage("survivor"), []int64{hiddenID}, "mirror-hidden")
			require.NoError(err)
			switch tc.timing {
			case "after discovery cursor":
				require.NoError(r.runBatch(t.Context()))
				assert.Equal(int32(1), r.Uploads.Load(), "dedup-hidden files upload before undo")
				assert.Zero(r.status().Reasons["source_unavailable"])
				restored, err := r.f.Store.UndoDedup("mirror-hidden")
				require.NoError(err)
				assert.Equal(int64(1), restored)
				require.NoError(r.runBatch(t.Context()))
				assert.Equal(int32(1), r.Uploads.Load())
				assert.Equal(int64(1), r.status().Delivered)
			case "between final page and completion":
				r.add("last.pdf", "application/pdf", []byte("last document"))
				var lastID int64
				require.NoError(r.f.Store.DB().QueryRow(`SELECT MAX(id) FROM attachments`).Scan(&lastID))
				restored := false
				r.worker.WithOperationGate(func(context.Context) (func(), bool) {
					scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
					require.NoError(err)
					if !restored && scan.AfterAttachmentID == lastID && scan.PassHighWater == lastID {
						count, err := r.f.Store.UndoDedup("mirror-hidden")
						require.NoError(err)
						require.Equal(int64(1), count)
						restored = true
					}
					return func() {}, true
				})
				require.NoError(r.runBatch(t.Context()))
				require.True(restored)
				scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
				require.NoError(err)
				assert.Equal(lastID, scan.AfterAttachmentID)
				intents, err := r.f.Store.SendingDocbankAttachments(t.Context(), "dest")
				require.NoError(err)
				assert.Empty(intents)
				r.worker.WithOperationGate(nil)
				require.NoError(r.runBatch(t.Context()))
				_, delivered := r.Uploaded.Load(hash)
				assert.True(delivered)
				assert.Equal(int32(2), r.Uploads.Load())
				assert.Equal(int64(2), r.status().Delivered)
			}
		})
	}
}

func TestAttachmentMirrorMalformedAliasReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	require.NoError(r.runBatch(t.Context()))
	hash := strings.Repeat("z", 64)
	mid := r.f.CreateMessage("malformed-alias")
	require.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "donor", SourcePartKey: "donor"}))
	require.NoError(r.f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{StoragePath: "zz/" + hash, SourceAttachmentID: "slack:alias", SourcePartKey: "alias", State: "stored"}))
	valid := r.add("valid.pdf", "application/pdf", []byte("valid document"))
	require.NoError(r.runBatch(t.Context()))
	_, delivered := r.Uploaded.Load(valid)
	assert.True(delivered)
	assert.Equal(int32(1), r.Uploads.Load())
	changes, err := r.f.Store.ListAttachmentChanges(t.Context(), store.DocbankAttachmentConsumerKey("dest"), 500)
	require.NoError(err)
	assert.Empty(changes)
}

func TestAttachmentMirrorBackfillDuringPass(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := newMirrorRig(t)
	r.add("first.pdf", "application/pdf", []byte("first document"))
	require.NoError(r.runBatch(t.Context()))
	r.add("second.pdf", "application/pdf", []byte("second document"))
	reset := false
	r.worker.WithOperationGate(func(context.Context) (func(), bool) {
		if !reset && r.Uploads.Load() == 2 {
			reset = true
			require.NoError(r.f.Store.RetryDocbankAttachments(t.Context(), "dest"))
		}
		return func() {}, true
	})
	require.NoError(r.runBatch(t.Context()))
	assert.True(reset)
	// The backfill route starts a follow-up run, which repeats discovery.
	require.NoError(r.runBatch(t.Context()))
	assert.Equal(int32(2), r.Uploads.Load())
	assert.Equal(int64(2), r.status().Delivered)
	scan, err := r.f.Store.LoadDocbankAttachmentScan(t.Context(), "dest")
	require.NoError(err)
	assert.Positive(scan.AfterAttachmentID)
	assert.True(scan.NextFullScanAt.After(time.Now()))
}
