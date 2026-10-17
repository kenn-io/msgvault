package cmd

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/scheduler"
	"log/slog"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCLISyncPathsShareOutagesAndIgnorePausedOrMaintenance(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var events []string
	adapter := &storeAPIAdapter{outcomes: telemetry.NewOutcomes(t.TempDir(), func(event string, _ map[string]any) error { events = append(events, event); return nil })}
	failure := errors.New("private provider details")
	result := failure
	paused := false
	syncRunner := func(_ context.Context, _ []string, emit func(string, string) error) error {
		if paused {
			require.NoError(emit("outcome", "paused"))
		}
		return result
	}
	commandRunner := func(ctx context.Context, _ []string, _ map[string]string, _ string, emit func(string, string) error) error {
		return syncRunner(ctx, nil, emit)
	}
	for range 2 {
		require.ErrorIs(adapter.runCLISyncOperationWithRunner(t.Context(), api.CLISyncRequest{}, nil, syncRunner), failure)
	}
	require.ErrorIs(adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-omi"}}, nil, commandRunner), failure)
	assert.Len(events, 1)
	paused, result = true, nil
	require.NoError(adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-omi"}}, nil, commandRunner))
	paused, result = false, failure
	require.ErrorIs(adapter.runCLISyncOperationWithRunner(t.Context(), api.CLISyncRequest{}, nil, syncRunner), failure)
	assert.Len(events, 1)
	result = nil
	require.NoError(adapter.runCLISyncOperationWithRunner(t.Context(), api.CLISyncRequest{}, nil, syncRunner))
	result = failure
	require.ErrorIs(adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-omi"}}, nil, commandRunner), failure)
	require.ErrorIs(adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-circleback", "--probe"}}, nil, commandRunner), failure)
	assert.Len(events, 2)
	fixture := storetest.New(t)
	adapter.store = fixture.Store
	source, err := fixture.Store.GetOrCreateSource("omi", "alpha")
	require.NoError(err)
	require.ErrorIs(adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-omi", "alpha"}}, nil, commandRunner), failure)
	require.ErrorIs(adapter.runCLISyncOperationWithRunner(t.Context(), api.CLISyncRequest{SourceID: source.ID, SourceIDSet: true}, nil, syncRunner), failure)
	assert.Len(events, 3)
	result = nil
	require.NoError(adapter.runCLISyncOperationWithRunner(t.Context(), api.CLISyncRequest{SourceID: source.ID, SourceIDSet: true}, nil, syncRunner))
	result = failure
	require.ErrorIs(adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-omi", "alpha"}}, nil, commandRunner), failure)
	assert.Len(events, 4)
}

func TestServeSchedulersTrackSourceOutagesAndExcludeMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		var reasons []any
		outcomes := telemetry.NewOutcomes(t.TempDir(), func(event string, properties map[string]any) error {
			assert.Equal("sync_failed", event)
			reasons = append(reasons, properties["reason"])
			return nil
		})
		result := errors.New("provider failed")
		sched, media := newServeSchedulers(func(context.Context, string) error { return result }, slog.New(slog.DiscardHandler), nil, nil, nil, outcomes)
		defer func() { <-sched.Stop().Done(); <-media.Stop().Done() }()
		require.NoError(sched.AddAccount("account@example.com", "0 0 1 1 *"))
		for _, name := range []string{"granola:archive", "cache-build", "carddav:contacts"} {
			require.NoError(sched.AddJob(scheduler.Job{Name: name, Schedule: "0 0 1 1 *", Run: func(context.Context) error { return result }}))
		}
		for range 2 {
			require.NoError(sched.TriggerSync("account@example.com", false))
			synctest.Wait()
			for _, name := range []string{"granola:archive", "cache-build", "carddav:contacts"} {
				_, err := sched.StartJob(name, false)
				require.NoError(err)
				synctest.Wait()
			}
		}
		assert.Equal([]any{"other", "other"}, reasons)
		result = nil
		require.NoError(sched.TriggerSync("account@example.com", false))
		synctest.Wait()
		result = errors.New("new outage")
		_, err := sched.StartJob("granola:archive", false)
		require.NoError(err)
		synctest.Wait()
		require.NoError(sched.TriggerSync("account@example.com", false))
		synctest.Wait()
		assert.Equal([]any{"other", "other", "other"}, reasons)
	})
}
