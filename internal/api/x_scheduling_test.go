package api_test

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXSchedulingThroughSourceStatus(t *testing.T) {
	st := testutil.NewTestStore(t)
	xSource, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	collision, err := st.GetOrCreateSource("discord", "1001")
	require.NoError(t, err)
	discord, err := st.GetOrCreateSource("discord", "2001")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "example-owner-key"
	var calls atomic.Int32
	done := make(chan string, 2)
	sched := scheduler.New(func(_ context.Context, id string) error { calls.Add(1); done <- id; return nil }).WithLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(t, sched.AddAccount("1001", "0 0 1 1 *"))
	require.NoError(t, sched.AddAccount("2001", "0 0 1 1 *"))
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Scheduler: sched, Logger: slog.New(slog.DiscardHandler)})
	request := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-API-Key", cfg.Server.APIKey)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		return response
	}
	response := request(http.MethodGet, "/api/v1/sources/status")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var status api.SourceStatusResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	require.Len(t, status.Sources, 3)
	byID := make(map[int64]api.SourceStatus)
	for _, source := range status.Sources {
		byID[source.ID] = source
	}
	assert.True(t, byID[xSource.ID].Scheduled)
	assert.True(t, byID[xSource.ID].CanSync)
	assert.False(t, byID[collision.ID].Scheduled, "Discord must not borrow the exact X owner's schedule")
	assert.False(t, byID[collision.ID].CanSync)
	assert.True(t, byID[discord.ID].Scheduled, "unambiguous existing providers keep their schedules")
	assert.True(t, byID[discord.ID].CanSync)
	response = request(http.MethodPost, "/api/v1/sync/1001?source_type=discord")
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Zero(t, calls.Load(), "an incompatible typed trigger must not reach the scheduler")
	for _, entry := range []struct{ id, kind string }{{"1001", "x"}, {"2001", "discord"}} {
		response = request(http.MethodPost, "/api/v1/sync/"+entry.id+"?source_type="+entry.kind)
		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
		select {
		case id := <-done:
			assert.Equal(t, entry.id, id)
		case <-time.After(30 * time.Second):
			require.FailNow(t, "scheduler did not execute")
		}
	}
	assert.Equal(t, int32(2), calls.Load())
}
