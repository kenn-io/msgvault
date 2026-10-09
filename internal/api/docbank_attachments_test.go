package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocbankAttachmentOpenAPIAuthentication(t *testing.T) {
	t.Parallel()
	doc := OpenAPIDocument()
	for _, tc := range []struct {
		path, method, operationID string
	}{
		{"/api/v1/integrations/docbank/attachments", http.MethodGet, "getDocbankAttachmentStatus"},
		{"/api/v1/integrations/docbank/attachments/backfill", http.MethodPost, "backfillDocbankAttachments"},
	} {
		t.Run(tc.operationID, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			op := pathOperation(doc.Paths[tc.path], tc.method)
			require.NotNil(op)
			assert.Equal(tc.operationID, op.OperationID)
			assert.Equal([]string{"API"}, op.Tags)
			require.Len(op.Security, 1)
			assert.Contains(op.Security[0], apiKeySecurityScheme)
		})
	}
}

func TestDocbankAttachmentBackfillConsentAndRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("backfill-message")
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "report.pdf", MIMEType: "application/pdf", ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	var id int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id=?"), mid).Scan(&id))
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank = config.DocbankIntegrationConfig{Enabled: true, URL: "https://docbank.example.com", AttachmentMirror: true}
	uid, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	destination := docbankmedia.AttachmentDestinationKey(cfg.Integrations.Docbank.URL, uid, "/msgvault")
	require.NoError(f.Store.SaveDocbankAttachmentScan(t.Context(), destination, "https://docbank.example.com", "/msgvault", []store.DocbankAttachmentCandidate{{ID: id, ContentHash: hash, Filename: "report.pdf", MIMEType: "application/pdf", Size: 20}}, []string{""}, 100))
	require.NoError(f.Store.CompleteDocbankAttachment(t.Context(), destination, hash, store.DocbankAttachmentRef{}, "forbidden", false))
	sched := newMockScheduler()
	sched.scheduledJobs = map[string]bool{scheduler.StoredMediaSubmitJob: true}
	server := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Scheduler: sched, Logger: slog.New(slog.DiscardHandler)})
	router := server.Router()
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/docbank/attachments/backfill", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	assert.Equal(http.StatusConflict, post().Code)
	_, ok, err := f.Store.NextDocbankAttachment(t.Context(), destination, store.DocbankAttachmentFilter{})
	require.NoError(err)
	assert.False(ok)
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	started, release := make(chan struct{}), make(chan struct{})
	realScheduler := scheduler.New(func(context.Context, string) error { return nil })
	defer func() { close(release); <-realScheduler.Stop().Done() }()
	require.NoError(realScheduler.AddJob(scheduler.Job{Name: scheduler.StoredMediaSubmitJob, Schedule: "0 0 1 1 *", Run: func(context.Context) error {
		close(started)
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
	_, ok, err = f.Store.NextDocbankAttachment(t.Context(), destination, store.DocbankAttachmentFilter{})
	require.NoError(err)
	assert.True(ok)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/docbank/attachments", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code)
	var status DocbankAttachmentStatus
	require.NoError(json.Unmarshal(w.Body.Bytes(), &status))
	assert.True(status.Enabled)
	assert.True(status.UploadConsent)
	assert.Equal("/msgvault", status.Collection)
	assert.Equal(int64(1), status.Counts.Failed)
}
