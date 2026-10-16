package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCLISourceMaintenanceRoutes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	srv := NewServer(&config.Config{}, &cliIdentityDiscoveryTestStore{Store: st}, nil, testLogger())
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		return response
	}
	response := post("/api/v1/cli/account", fmt.Sprintf(`{"source_id":%d,"alias":"recovered-history","history_only":true}`, from.ID))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var updated struct {
		Alias      string `json:"alias"`
		Identifier string `json:"identifier"`
	}
	require.NoError(json.Unmarshal(response.Body.Bytes(), &updated))
	assert.Equal("recovered-history", updated.Alias)
	assert.Equal(from.Identifier, updated.Identifier)
	settings, err := st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Equal("recovered-history", settings.Alias)
	assert.True(settings.HistoryOnly)
	response = post("/api/v1/cli/account/merge", `{"from":"recovered-history","into":"live-example","dry_run":true}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var preview store.SourceMergeResult
	require.NoError(json.Unmarshal(response.Body.Bytes(), &preview))
	assert.Equal(from.ID, preview.FromSourceID)
	assert.Equal(into.ID, preview.IntoSourceID)
	assert.True(preview.DryRun)
	settings, err = st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Zero(settings.MergedIntoSourceID)
	response = post("/api/v1/cli/account/merge", fmt.Sprintf(`{"from_source_id":%d,"into_source_id":%d}`, from.ID, into.ID))
	require.Equal(http.StatusBadRequest, response.Code, "dry_run must be explicit: %s", response.Body.String())
	settings, err = st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Zero(settings.MergedIntoSourceID)
	response = post("/api/v1/cli/account/merge", fmt.Sprintf(`{"from_source_id":%d,"into_source_id":%d,"dry_run":null}`, from.ID, into.ID))
	require.Equal(http.StatusBadRequest, response.Code, "null is not a merge decision: %s", response.Body.String())
	response = post("/api/v1/cli/account/merge", fmt.Sprintf(`{"from_source_id":%d,"into_source_id":%d,"dry_run":false}`, from.ID, into.ID))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	settings, err = st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Equal(into.ID, settings.MergedIntoSourceID)
}

func TestCLISourceMergeSchedulesCacheRebuildOnlyAfterNewCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		from, err := st.GetOrCreateSource("beeper", "history-example")
		require.NoError(err)
		into, err := st.GetOrCreateSource("beeper", "live-example")
		require.NoError(err)
		wrapped := &stubIdentityRebuildStore{Store: st, buildCh: make(chan bool, 2)}
		srv := NewServer(&config.Config{}, wrapped, nil, testLogger())
		defer func() {
			require.NoError(srv.Shutdown(context.Background()))
			synctest.Wait()
		}()

		post := func(body string) *httptest.ResponseRecorder {
			t.Helper()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/account/merge", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			return response
		}
		body := fmt.Sprintf(`{"from_source_id":%d,"into_source_id":%d`, from.ID, into.ID)

		response := post(body + `,"dry_run":true}`)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		synctest.Wait()
		assertNoPendingCacheBuild(t, wrapped)

		response = post(body + `,"dry_run":false}`)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		assert.False(requireCacheBuild(t, wrapped), "the cache builder rechecks drift and chooses the necessary rebuild mode")

		response = post(body + `,"dry_run":false}`)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var retry store.SourceMergeResult
		require.NoError(json.Unmarshal(response.Body.Bytes(), &retry))
		assert.True(retry.AlreadyMerged)
		synctest.Wait()
		assertNoPendingCacheBuild(t, wrapped)
	})
}
