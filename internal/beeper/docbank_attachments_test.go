package beeper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Removing content deduplication, consent gates, or durable retry state breaks this test.
func TestAttachmentMirrorDelivery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	f := storetest.New(t)
	data := []byte("synthetic document")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dir, hash[:2]), 0700))
	require.NoError(os.WriteFile(filepath.Join(dir, hash[:2], hash), data, 0600))
	var attachmentIDs []int64
	for i := range 2 {
		mid := f.CreateMessage(fmt.Sprintf("mirror-%d", i))
		filename := "report.pdf"
		if i == 1 {
			filename = "renamed.pdf"
		}
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: filename, MIMEType: "application/pdf", ContentHash: hash, Size: int64(len(data)), StoragePath: hash[:2] + "/" + hash, SourceAttachmentID: fmt.Sprintf("file-%d", i), State: "stored"}))
		var id int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id = ?"), mid).Scan(&id))
		attachmentIDs = append(attachmentIDs, id)
	}
	blobs, err := attachmentstore.New(store.NewPackCatalog(f.Store), dir)
	require.NoError(err)
	defer func() { assert.NoError(blobs.Close()) }()
	var requests atomic.Int32
	var uploads atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/v1/path" {
			_, _ = io.WriteString(w, `{"id":7,"kind":"dir"}`)
			return
		}
		if r.URL.Path != "/api/v1/uploads" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		uploads.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		parts, err := r.MultipartReader()
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := parts.NextPart()
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(part)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(data, body)
		assert.Equal(hash+"-report.pdf", part.FileName())
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(docbankmedia.AttachmentReceipt{Status: "added", ComputedHash: hash, ComputedSize: 18, Node: docbankmedia.AttachmentNode{ID: 8, ParentID: 7, Kind: "file", Name: part.FileName(), BlobHash: hash, Size: 18, VersionID: "version"}}))
	}))
	defer srv.Close()
	cfg := config.DocbankIntegrationConfig{Enabled: true, URL: srv.URL, AttachmentMirror: true}
	client, err := docbankmedia.NewClient(srv.URL, nil)
	require.NoError(err)
	worker := NewAttachmentMirror(f.Store, blobs, client, "destination", cfg)
	_, err = worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(uploads.Load())
	assert.Zero(requests.Load())
	status, err := f.Store.DocbankAttachmentStatus(t.Context(), "destination")
	require.NoError(err)
	assert.Equal(int64(2), status.Pending)
	cfg.AttachmentUploadConsent = true
	cfg.AttachmentMIMEClasses = []string{"image"}
	require.NoError(f.Store.RetryDocbankAttachments(t.Context(), "destination"))
	worker = NewAttachmentMirror(f.Store, blobs, client, "destination", cfg)
	_, err = worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(requests.Load())
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "destination")
	require.NoError(err)
	assert.Equal(int64(2), status.Skipped)
	assert.Equal(int64(2), status.Reasons["mime_filtered"])
	cfg.AttachmentMIMEClasses = nil
	require.NoError(f.Store.RetryDocbankAttachments(t.Context(), "destination"))
	worker = NewAttachmentMirror(f.Store, blobs, client, "destination", cfg)
	_, err = worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(int32(1), uploads.Load())
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "destination")
	require.NoError(err)
	assert.Equal(int64(2), status.Failed)
	fail.Store(false)
	require.NoError(f.Store.RetryDocbankAttachments(t.Context(), "destination"))
	worker = NewAttachmentMirror(f.Store, blobs, client, "destination", cfg) // restart
	_, err = worker.RunBatch(t.Context())
	require.NoError(err)
	for range 3 {
		_, err = worker.RunBatch(t.Context())
		require.NoError(err)
	}
	assert.Equal(int32(2), uploads.Load())
	status, err = f.Store.DocbankAttachmentStatus(t.Context(), "destination")
	require.NoError(err)
	assert.Equal(int64(2), status.Delivered)
	for _, id := range attachmentIDs {
		refs, err := f.Store.DocbankAttachmentRefs(t.Context(), id)
		require.NoError(err)
		require.Len(refs, 1)
		assert.Equal(int64(8), refs[0].NodeID)
		assert.Equal(hash, refs[0].BlobHash)
	}
}

func TestAttachmentMirrorFilters(t *testing.T) {
	t.Parallel()
	candidate := store.DocbankAttachmentCandidate{MIMEType: "application/pdf", SourceID: 3, Size: 20, SentAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StoragePath: "aa/file"}
	for _, tc := range []struct {
		name   string
		cfg    config.DocbankIntegrationConfig
		reason string
	}{
		{"all", config.DocbankIntegrationConfig{}, ""},
		{"mime", config.DocbankIntegrationConfig{AttachmentMIMEClasses: []string{"image"}}, "mime_filtered"},
		{"source", config.DocbankIntegrationConfig{AttachmentSourceIDs: []int64{4}}, "source_filtered"},
		{"size", config.DocbankIntegrationConfig{AttachmentMaxBytes: 19}, "size_filtered"},
		{"date", config.DocbankIntegrationConfig{AttachmentAfter: "2026-01-03"}, "date_filtered"},
	} {
		t.Run(tc.name, func(t *testing.T) { t.Parallel(); assert.Equal(t, tc.reason, attachmentSkipReason(candidate, tc.cfg)) })
	}
}

func TestAttachmentMirrorCurrentFiltersBlockPendingUpload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("pending-filter")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "report.pdf", MIMEType: "application/pdf", ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	var id int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id=?"), mid).Scan(&id))
	require.NoError(f.Store.SaveDocbankAttachmentScan(t.Context(), "destination", "https://docbank.example.com", "/msgvault", []store.DocbankAttachmentCandidate{{ID: id, ContentHash: hash, Filename: "report.pdf", MIMEType: "application/pdf", Size: 20}}, []string{""}, 100))
	delivery, ok, err := f.Store.NextDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentFilter{MaxBytes: 19})
	require.NoError(err)
	assert.False(ok)
	assert.Empty(delivery.ContentHash)
}

func TestAttachmentMirrorDiscoveryKeepsRescanDeadline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	f := storetest.New(t)
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var candidates []store.DocbankAttachmentCandidate
	for i := range 2 {
		mid := f.CreateMessage(fmt.Sprintf("rescan-%d", i))
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "report.pdf", ContentHash: hash, Size: 20, StoragePath: "aa/" + hash}))
		var id int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id=?"), mid).Scan(&id))
		candidates = append(candidates, store.DocbankAttachmentCandidate{ID: id, ContentHash: hash, Filename: "report.pdf", Size: 20})
	}
	require.NoError(f.Store.SaveDocbankAttachmentScan(t.Context(), "destination", "https://docbank.example.com", "/msgvault", candidates[:1], []string{""}, 100))
	deadline := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE docbank_attachment_scans SET next_scan_at=? WHERE destination_key=?"), deadline, "destination")
	require.NoError(err)
	require.NoError(f.Store.SaveDocbankAttachmentScan(t.Context(), "destination", "https://docbank.example.com", "/msgvault", candidates[1:], []string{""}, 100))
	var next time.Time
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT next_scan_at FROM docbank_attachment_scans WHERE destination_key=?"), "destination").Scan(&next))
	assert.True(deadline.Equal(next), "new arrivals must not postpone revisiting older attachments")
}

func TestAttachmentMirrorBusyGateEndsPass(t *testing.T) {
	t.Parallel()
	cfg := config.DocbankIntegrationConfig{Enabled: true, AttachmentMirror: true}
	worker := NewAttachmentMirror(nil, nil, nil, "destination", cfg).WithOperationGate(func(context.Context) (func(), bool) { return nil, false })
	_, err := worker.RunBatch(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = worker.RunBatch(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
