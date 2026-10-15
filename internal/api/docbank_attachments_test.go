package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocbankAttachmentBackfillConsentAndRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("backfill-message")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "report.pdf", MIMEType: "application/pdf", ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank = config.DocbankIntegrationConfig{Enabled: true, URL: "https://docbank.example.com"}
	destination := "destination"
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), destination, store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "forbidden"))
	sched := newMockScheduler()
	sched.scheduledJobs = map[string]bool{DocbankAttachmentMirrorJob: true}
	reader := &DocbankAttachmentReader{Store: f.Store, Destination: destination, Folder: "/msgvault"}
	server := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Scheduler: sched, Logger: slog.New(slog.DiscardHandler)})
	router := server.Router()
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/docbank/attachments/backfill", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	assert.Equal(http.StatusConflict, post().Code)
	due, err := f.Store.SendingDocbankAttachments(t.Context(), destination)
	require.NoError(err)
	assert.Empty(due)
	server.docbankAttachments = reader
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	cfg.Integrations.Docbank.Enabled = false
	assert.Equal(http.StatusConflict, post().Code)
	cfg.Integrations.Docbank.Enabled = true
	server.docbankAttachments = nil
	response := post()
	assert.Equal(http.StatusServiceUnavailable, response.Code)
	assert.Contains(response.Body.String(), "attachment mirror store unavailable")
	server.docbankAttachments = reader
	sched.scheduledJobs[DocbankAttachmentMirrorJob] = false
	assert.Equal(http.StatusServiceUnavailable, post().Code)
	sched.scheduledJobs[DocbankAttachmentMirrorJob] = true
	started, release := make(chan struct{}), make(chan struct{})
	realScheduler := scheduler.New(func(context.Context, string) error { return nil })
	defer func() { close(release); <-realScheduler.Stop().Done() }()
	require.NoError(realScheduler.AddJob(scheduler.Job{Name: DocbankAttachmentMirrorJob, Schedule: "0 0 1 1 *", Run: func(context.Context) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return nil
	}}))
	sched.startJobFn = realScheduler.StartJob
	sched.triggerJobFn = realScheduler.TriggerJob
	returned := make(chan *httptest.ResponseRecorder, 1)
	go func() { returned <- post() }()
	select {
	case response := <-returned:
		assert.Equal(http.StatusAccepted, response.Code)
	case <-time.After(5 * time.Second):
		require.FailNow("backfill must return while the upload job is blocked")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow("backfill did not start the stored-media job")
	}
	assert.Equal(http.StatusAccepted, post().Code)
	due, err = f.Store.SendingDocbankAttachments(t.Context(), destination)
	require.NoError(err)
	assert.Len(due, 1)
}

func TestDocbankAttachmentStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/docbank/attachments", nil))
		return w
	}
	response := get()
	require.Equal(http.StatusOK, response.Code)
	var disabled DocbankAttachmentStatus
	require.NoError(json.Unmarshal(response.Body.Bytes(), &disabled))
	assert.False(disabled.UploadConsent)
	assert.Equal("docbank_disabled", disabled.State)
	assert.Nil(disabled.Counts)
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	response = get()
	require.Equal(http.StatusOK, response.Code)
	require.NoError(json.Unmarshal(response.Body.Bytes(), &disabled))
	assert.True(disabled.UploadConsent)
	assert.Equal("docbank_disabled", disabled.State)
	cfg.Integrations.Docbank.AttachmentUploadConsent = false
	cfg.Integrations.Docbank.Enabled = true
	response = get()
	require.Equal(http.StatusOK, response.Code)
	var off DocbankAttachmentStatus
	require.NoError(json.Unmarshal(response.Body.Bytes(), &off))
	assert.Equal("consent_off", off.State)
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	response = get()
	assert.Equal(http.StatusServiceUnavailable, response.Code)
	assert.Contains(response.Body.String(), "attachment mirror store unavailable")

	f := storetest.New(t)
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("status"), store.AttachmentWrite{ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "forbidden"))
	sched := newMockScheduler()
	sched.scheduledJobs = map[string]bool{DocbankAttachmentMirrorJob: true}
	server = NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Scheduler: sched, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Store: f.Store, Destination: "destination", Folder: "/msgvault"}
	get = func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/docbank/attachments", nil))
		return w
	}
	var running DocbankAttachmentStatus
	require.NoError(json.Unmarshal(get().Body.Bytes(), &running))
	assert.Equal("running", running.State)
	assert.True(running.UploadConsent)
	assert.Equal("/msgvault", running.Folder)
	require.NotNil(running.Counts)
	assert.Equal(int64(1), running.Counts.Failed)
	assert.Zero(running.Counts.Remaining)
	sched.scheduledJobs[DocbankAttachmentMirrorJob] = false
	var down DocbankAttachmentStatus
	require.NoError(json.Unmarshal(get().Body.Bytes(), &down))
	assert.True(down.UploadConsent, "consent comes from config")
	assert.Equal("worker_unavailable", down.State)
	assert.Nil(down.Counts)
}

func TestDocbankAttachmentFileReceipt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	hash := strings.Repeat("a", 64)
	mid := f.CreateMessage("alias")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	ref := store.DocbankAttachmentRef{Endpoint: "https://docbank.example.com", Folder: "/msgvault", NodeID: 8, VersionID: "v1", BlobHash: hash}
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, ref, ""))
	var id int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id=?"), mid).Scan(&id))
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, Logger: slog.New(slog.DiscardHandler)})
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/files/%d", id), nil))
	require.Equal(http.StatusOK, response.Code)
	var file FileMetadataResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &file))
	assert.Equal(hash, file.ContentHash)
	assert.Equal([]store.DocbankAttachmentRef{ref}, file.Docbank)
}
