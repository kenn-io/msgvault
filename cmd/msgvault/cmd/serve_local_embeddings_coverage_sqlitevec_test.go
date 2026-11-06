//go:build fts5 && sqlite_vec

package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestLocalEmbeddingsCompatibleSwitchFailureOffersCoverageRecovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newLocalActivationFixture(t, "nomic-embed-text", "\n[vector.embed.schedule]\ncron = \"\"\nrun_after_sync = false\n")
	f.start(t)
	_, _, err := testutil.CreateIndexedSourceMessage(f.store, "archive@example.test", "pending", "Pear", "A pear tree is growing.")
	require.NoError(err)
	_, err = f.store.DB().Exec("UPDATE messages SET sent_at = '2026-01-03 12:00:00'")
	require.NoError(err)
	_, err = buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	require.NoError(err)
	engine, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.store.DB())
	require.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	f.server.SetAnalyticsEngine(engine, api.AnalyticsModeDuckDB)
	coverageStatus := func() api.SearchCoverageResponse {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/search/coverage", strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer owner")
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		f.server.Router().ServeHTTP(response, request)
		assert.Equal(http.StatusOK, response.Code, response.Body.String())
		var coverage api.SearchCoverageResponse
		assert.NoError(json.Unmarshal(response.Body.Bytes(), &coverage))
		return coverage
	}
	beforeBuild := f.runner.beforeBuild
	f.runner.beforeBuild = func() {
		beforeBuild()
		coverage := coverageStatus()
		assert.Equal(api.SearchCoverageIncomplete, coverage.Status)
		assert.Empty(coverage.Actions, "an active build already owns recovery")
	}
	f.runner.failBuild = true
	f.enableThroughAPI(t)
	require.Eventually(func() bool { return !f.localStatus(t).Running }, 5*time.Second, 10*time.Millisecond)

	coverage := coverageStatus()
	require.Equal(api.SearchCoverageIncomplete, coverage.Status)
	require.Equal(int64(2), coverage.EligibleCount)
	require.Equal(int64(1), coverage.EmbeddedCount)
	require.Contains(coverage.Actions, api.SearchCoverageActionBuildIndex)
	messages, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
	require.NoError(err)
	require.Len(messages, 1, "the existing index remains searchable after setup fails")
}
