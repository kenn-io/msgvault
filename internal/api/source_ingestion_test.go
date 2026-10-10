package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceStatusSeparatesIngestionEvidenceFromSuccessfulSync(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		finish               string
		itemError            bool
	}{
		{"never synced", "unknown", "provider_completeness_unverified", "", false},
		{"successful recent sync", "unknown", "provider_completeness_unverified", "completed", false},
		{"failed attempt", "partial", "sync_reported_errors", "failed", false},
		{"completed with item error", "partial", "sync_reported_errors", "completed", true},
		{"running", "unknown", "sync_in_progress", "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "sender@example.com")
			requirements.NoError(err)
			if tc.finish != "" {
				id, err := st.StartSync(source.ID, "full")
				requirements.NoError(err)
				if tc.itemError {
					requirements.NoError(st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: id, SourceMessageID: "synthetic-item", Phase: "ingest", Status: store.SyncRunItemStatusError, ErrorKind: "parse_error", ErrorMessage: "synthetic parser refusal"}))
				}
				switch tc.finish {
				case "completed":
					requirements.NoError(st.CompleteSync(id, "synthetic-cursor"))
					requirements.NoError(st.UpdateSourceSyncCursor(source.ID, "synthetic-cursor"))
				case "failed":
					requirements.NoError(st.FailSync(id, "synthetic failed attempt"))
				}
			}
			srv := NewServer(&config.Config{}, st, nil, testLogger())
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/sources/status", nil))
			requirements.Equal(http.StatusOK, response.Code)
			var body struct {
				Sources []struct {
					LastSyncAt        *string `json:"last_sync_at"`
					ProviderIngestion struct {
						Status string `json:"status"`
						Reason string `json:"reason"`
					} `json:"provider_ingestion"`
				} `json:"sources"`
			}
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			requirements.Len(body.Sources, 1)
			assertions.Equal(tc.status, body.Sources[0].ProviderIngestion.Status)
			assertions.Equal(tc.reason, body.Sources[0].ProviderIngestion.Reason)
			if tc.finish == "completed" {
				assertions.NotNil(body.Sources[0].LastSyncAt, "a recent successful cursor does not prove provider completeness")
			}
		})
	}
}
