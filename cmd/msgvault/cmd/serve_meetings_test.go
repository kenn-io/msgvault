package cmd

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreAPIAdapterServesMeetingMetricsThroughRealDaemonRoute(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/metrics", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	srv.Router().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	archiveUID, err := st.ArchiveUID()
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"schema_version":1,
		"archive_uid":"`+archiveUID+`",
		"totals":{"meeting_count":0,"known_duration_count":0,"unknown_duration_count":0,"total_known_seconds":0,"average_known_seconds":null},
		"first_meeting_at":null,
		"last_meeting_at":null,
		"undated_count":0,
		"duration_by_basis":[],
		"months":[],
		"scope":{"kind":"direct"}
	}`, response.Body.String())
}

var _ api.MeetingStore = (*storeAPIAdapter)(nil)
