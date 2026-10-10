package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type recordingReferenceTestGate struct {
	*api.SerialOperationGate

	labels chan string
}

func (g *recordingReferenceTestGate) BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool) {
	g.labels <- label
	return g.SerialOperationGate.BeginLabeledWorkContext(ctx, label)
}

func TestRecordingReferenceJob(t *testing.T) {
	t.Run("consent", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		s := scheduler.New(nil)
		defer func() { <-s.Stop().Done() }()
		for _, cfg := range []config.DocbankIntegrationConfig{{}, {Enabled: true}, {Enabled: true, AllSourcesUploadConsent: true}, {ReferenceConsent: true}} {
			require.NoError(configureRecordingReferenceJob(t.Context(), s, nil, f.Store, cfg, nil))
			assert.False(s.IsJobScheduled(recordingReferenceJob))
		}
	})
	t.Run("HTTP releases store gate", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := storetest.New(t)
		id := f.CreateMessage("recording")
		require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "https://loom.com/share/abc", Valid: true}, sql.NullString{}))
		_, err := f.Store.DB().Exec(`UPDATE messages SET content_changed_at='2000-01-01 00:00:00.000'`)
		require.NoError(err)
		arrived, release := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req docbankmedia.ReferenceRequest
			if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
				return
			}
			assert.False(req.Acquire)
			close(arrived)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			data, _ := json.Marshal(docbankmedia.Receipt{VaultUID: "vault", SourceID: "source", OperationID: req.OperationID, OccurrenceID: "occurrence", OperationState: "succeeded", CoverageState: "unprocessed", Outcome: "access_required"})
			_, _ = w.Write(data)
		}))
		defer server.Close()
		t.Setenv("MSGVAULT_TEST_REFERENCE_KEY", "synthetic-key")
		gate := &recordingReferenceTestGate{SerialOperationGate: api.NewSerialOperationGate(), labels: make(chan string, 20)}
		s := scheduler.New(nil)
		defer func() { close(release); <-s.Stop().Done() }()
		require.NoError(configureRecordingReferenceJob(t.Context(), s, gate, f.Store, config.DocbankIntegrationConfig{Enabled: true, ReferenceConsent: true, URL: server.URL, APIKeyEnv: "MSGVAULT_TEST_REFERENCE_KEY"}, nil))
		assert.True(s.IsJobScheduled(recordingReferenceJob))
		done := make(chan error, 1)
		go func() { done <- s.TriggerJob(recordingReferenceJob) }()
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			require.FailNow("recording reference request did not arrive")
		}
		require.NotEmpty(gate.labels)
		for len(gate.labels) > 0 {
			assert.Equal("Recording reference submission", <-gate.labels)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		unlock, ok := gate.BeginLabeledWorkContext(ctx, "provider sync")
		require.True(ok)
		require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "edited while sending", Valid: true}, sql.NullString{}))
		unlock()
	})
}
