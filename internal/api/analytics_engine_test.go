package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type blockingAnalyticsEngine struct {
	*querytest.MockEngine

	entered chan struct{}
	release chan struct{}
}

func (e *blockingAnalyticsEngine) Aggregate(
	ctx context.Context,
	_ query.ViewType,
	_ query.AggregateOptions,
) ([]query.AggregateRow, error) {
	close(e.entered)
	select {
	case <-e.release:
		return e.AggregateRows, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAnalyticsEngineSwapUpdatesHealthAndHandlers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldEngine := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{{Key: "old", Count: 1}},
	}
	newEngine := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{{Key: "new", Count: 2}},
	}
	opts := testServerOptions(t, nil)
	opts.Engine = oldEngine
	opts.AnalyticsMode = AnalyticsModeSQLFallback
	srv := NewServerWithOptions(opts)

	assertHealthMode := func(want string) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		require.Equal(http.StatusOK, rec.Code)
		var body HealthResponse
		require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(want, body.AnalyticsEngine)
	}

	assertHealthMode(AnalyticsModeSQLFallback)
	srv.SetAnalyticsEngine(newEngine, AnalyticsModeDuckDB)
	assertHealthMode(AnalyticsModeDuckDB)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/aggregates?view_type=senders", nil))
	require.Equal(http.StatusOK, rec.Code, rec.Body.String())
	var body AggregateResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(body.Rows, 1)
	assert.Equal("new", body.Rows[0].Key)
}

func TestAnalyticsEngineSwapDoesNotBlockHealthWhileRequestRuns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldEngine := &blockingAnalyticsEngine{
		MockEngine: &querytest.MockEngine{AggregateRows: []query.AggregateRow{{Key: "old", Count: 1}}},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	opts := testServerOptions(t, nil)
	opts.Engine = oldEngine
	opts.AnalyticsMode = AnalyticsModeSQLFallback
	srv := NewServerWithOptions(opts)
	t.Cleanup(func() {
		select {
		case <-oldEngine.release:
		default:
			close(oldEngine.release)
		}
	})

	aggregateDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(
			http.MethodGet,
			"/api/v1/aggregates?view_type=senders",
			nil,
		))
		aggregateDone <- rec
	}()
	select {
	case <-oldEngine.entered:
	case <-time.After(time.Second):
		require.FailNow("aggregate request did not enter the old engine")
	}

	swapDone := make(chan struct{})
	go func() {
		srv.SetAnalyticsEngine(
			&querytest.MockEngine{AggregateRows: []query.AggregateRow{{Key: "new", Count: 2}}},
			AnalyticsModeDuckDB,
		)
		close(swapDone)
	}()
	require.Eventually(func() bool {
		select {
		case <-swapDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "engine swap waited for an in-flight request")

	health := httptest.NewRecorder()
	srv.Router().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(http.StatusOK, health.Code)
	var healthBody HealthResponse
	require.NoError(json.Unmarshal(health.Body.Bytes(), &healthBody))
	assert.Equal(AnalyticsModeDuckDB, healthBody.AnalyticsEngine)

	close(oldEngine.release)
	oldResponse := <-aggregateDone
	require.Equal(http.StatusOK, oldResponse.Code, oldResponse.Body.String())
	var oldBody AggregateResponse
	require.NoError(json.Unmarshal(oldResponse.Body.Bytes(), &oldBody))
	require.Len(oldBody.Rows, 1)
	assert.Equal("old", oldBody.Rows[0].Key,
		"the in-flight request must keep its original engine snapshot")
}
