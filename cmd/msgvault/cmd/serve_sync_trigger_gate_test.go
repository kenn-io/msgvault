package cmd

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

// TestSyncTriggerAnsweredWhileScheduledRunHoldsGate wires the scheduler and the
// API server to one operation gate exactly as serve does (newServeSchedulers),
// lets a scheduled run of a generic job hold the gate, and triggers the same
// job over HTTP. The trigger must be answered 202 "pending" immediately: it
// only reserves a follow-up run, so it must neither wait on the gate nor
// register as a request waiter that makes the running job yield.
func TestSyncTriggerAnsweredWhileScheduledRunHoldsGate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const job = "granola:acct-1"
	gate := api.NewSerialOperationGate()
	sched, _ := newServeSchedulers(nil, slog.New(slog.DiscardHandler), nil, gate)
	defer func() { <-sched.Stop().Done() }()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	require.NoError(sched.AddJob(scheduler.Job{
		Name:     job,
		Schedule: "0 0 1 1 *",
		Run: func(ctx context.Context) error {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}))
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Scheduler:     sched,
		Logger:        slog.New(slog.DiscardHandler),
		OperationGate: gate,
	})
	trigger := func() *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/v1/sync/acct-1?source_type=granola", nil))
		return resp
	}

	require.Equal(http.StatusAccepted, trigger().Code, "first trigger starts the run")
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		require.FailNow("scheduled run did not start")
	}
	_, _, held := gate.Holder()
	require.True(held, "the active run holds the shared gate")

	begin := time.Now()
	resp := trigger()

	require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	assert.Contains(resp.Body.String(), `"disposition":"pending"`)
	assert.Less(time.Since(begin), 2*time.Second, "trigger must not wait on the gate")
	assert.False(gate.HasRequestWaiters(), "trigger must not register as a request waiter")

	close(release)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		require.FailNow("queued follow-up run did not start")
	}
}
