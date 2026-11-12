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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	xSource, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	collision, err := st.GetOrCreateSource("discord", "1001")
	_testRequire.NoError(err)
	discord, err := st.GetOrCreateSource("discord", "2001")
	_testRequire.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "example-owner-key"
	var calls atomic.Int32
	done := make(chan string, 2)
	sched := scheduler.New(func(_ context.Context, id string) error { calls.Add(1); done <- id; return nil }).WithLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { <-sched.Stop().Done() })
	_testRequire.NoError(sched.AddAccount("1001", "0 0 1 1 *"))
	_testRequire.NoError(sched.AddAccount("2001", "0 0 1 1 *"))
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Scheduler: sched, Logger: slog.New(slog.DiscardHandler)})
	request := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		req.Header.Set("X-Api-Key", cfg.Server.APIKey)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		return response
	}
	response := request(http.MethodGet, "/api/v1/sources/status")
	_testRequire.Equal(http.StatusOK, response.Code, response.Body.String())
	var status api.SourceStatusResponse
	_testRequire.NoError(json.Unmarshal(response.Body.Bytes(), &status))
	_testRequire.Len(status.Sources, 3)
	byID := make(map[int64]api.SourceStatus)
	for _, source := range status.Sources {
		byID[source.ID] = source
	}
	_testAssert.True(byID[xSource.ID].Scheduled)
	_testAssert.True(byID[xSource.ID].CanSync)
	_testAssert.False(byID[collision.ID].Scheduled, "Discord must not borrow the exact X owner's schedule")
	_testAssert.False(byID[collision.ID].CanSync)
	_testAssert.True(byID[discord.ID].Scheduled, "unambiguous existing providers keep their schedules")
	_testAssert.True(byID[discord.ID].CanSync)
	response = request(http.MethodPost, "/api/v1/sync/1001?source_type=discord")
	_testRequire.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	_testAssert.Zero(calls.Load(), "an incompatible typed trigger must not reach the scheduler")
	for _, entry := range []struct{ id, kind string }{{"1001", "x"}, {"2001", "discord"}} {
		response = request(http.MethodPost, "/api/v1/sync/"+entry.id+"?source_type="+entry.kind)
		_testRequire.Equal(http.StatusAccepted, response.Code, response.Body.String())
		select {
		case id := <-done:
			_testAssert.Equal(entry.id, id)
		case <-time.After(30 * time.Second):
			_testRequire.FailNow("scheduler did not execute")
		}
	}
	_testAssert.Equal(int32(2), calls.Load())
}
