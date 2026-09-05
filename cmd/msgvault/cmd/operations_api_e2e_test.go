package cmd

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestOperationHistoryAPIServesThroughProductionAdapter protects the daemon
// seam: serve.go passes storeAPIAdapter, never *store.Store, to both API
// capabilities used by archive-bound operation history.
func TestOperationHistoryAPIServesThroughProductionAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "private-operation-owner@example.invalid")
	require.NoError(err)
	startedAt := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO sync_runs (
		id, source_id, started_at, completed_at, status, messages_processed,
		messages_added, messages_updated, errors_count, error_message
	) VALUES (42424242, ?, ?, ?, 'completed', 3, 1, 1, 0, ?)`,
		source.ID, startedAt.Format("2006-01-02 15:04:05"),
		startedAt.Add(time.Second).Format("2006-01-02 15:04:05"),
		"private-operation-error")
	require.NoError(err)

	adapter := &storeAPIAdapter{store: st}
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: adapter, OperationHistoryReader: adapter,
		Logger: slog.New(slog.DiscardHandler),
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	response, err := http.Get(httpSrv.URL + "/api/v1/operations/runs?kind=source_sync")
	require.NoError(err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(http.StatusOK, response.StatusCode)
	listBody, err := io.ReadAll(response.Body)
	require.NoError(err)
	archiveUID, err := adapter.ArchiveUIDContext(t.Context())
	require.NoError(err)
	assert.NotContains(string(listBody), "private-operation-owner")
	assert.NotContains(string(listBody), "private-operation-error")
	assert.NotContains(string(listBody), archiveUID)
	assert.NotContains(string(listBody), "42424242")
	var page api.OperationRunsResponse
	require.NoError(json.Unmarshal(listBody, &page))
	require.Len(page.Runs, 1)
	assert.Equal(operations.KindSourceSync, page.Runs[0].Kind)
	assert.True(strings.HasPrefix(page.Runs[0].ID, "op2."))
	assert.NotContains(page.Runs[0].ID, "source_sync")

	detailResponse, err := http.Get(httpSrv.URL + "/api/v1/operations/runs/" + page.Runs[0].ID)
	require.NoError(err)
	defer func() { _ = detailResponse.Body.Close() }()
	require.Equal(http.StatusOK, detailResponse.StatusCode)
	detailBody, err := io.ReadAll(detailResponse.Body)
	require.NoError(err)
	assert.NotContains(string(detailBody), "private-operation-owner")
	assert.NotContains(string(detailBody), "private-operation-error")
	assert.NotContains(string(detailBody), archiveUID)
	assert.NotContains(string(detailBody), "42424242")
	var detail api.OperationRunDetail
	require.NoError(json.Unmarshal(detailBody, &detail))
	assert.Equal(page.Runs[0], detail.OperationRunSummary)
}
