package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRapidEmailRejectsNonCanonicalTargetsBeforeCredentials(t *testing.T) {
	for _, scenario := range []string{"missing", "display name", "ambiguous providers"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
`), 0o600))
			cfg, err := config.Load(path, dir)
			require.NoError(t, err)
			st := storetest.New(t)
			switch scenario {
			case "display name":
				src, err := st.Store.GetOrCreateSource("gmail", "other@example.test")
				require.NoError(t, err)
				require.NoError(t, st.Store.UpdateSourceDisplayName(src.ID, "sample@example.test"))
			case "ambiguous providers":
				for _, typ := range []string{"gmail", "msmail"} {
					_, err := st.Store.GetOrCreateSource(typ, "sample@example.test")
					require.NoError(t, err)
				}
			}
			count := func() int {
				t.Helper()
				var n int
				require.NoError(t, st.Store.DB().QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n))
				return n
			}
			before := count()
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			credentials := 0
			err = runScheduledSync(ctx, "sample@example.test", st.Store, func(context.Context, string) (*oauth.Manager, error) {
				credentials++
				return nil, errors.New("synthetic credentials unavailable")
			}, invocationFromContext(ctx))
			assert.Error(t, err)
			assert.Zero(t, credentials, "invalid rapid targets fail before resolving credentials")
			assert.Equal(t, before, count(), "rapid reconciliation never creates a source")
		})
	}
}

func TestRapidEmailResolverRejectsAliasAndRetiredSource(t *testing.T) {
	f := storetest.New(t)
	old, err := f.Store.GetOrCreateSource("gmail", "old@example.test")
	require.NoError(t, err)
	current, err := f.Store.GetOrCreateSource("gmail", "current@example.test")
	require.NoError(t, err)
	alias := "primary-mailbox"
	_, err = f.Store.UpdateSourceSettingsContext(t.Context(), old.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.NoError(t, err)
	_, err = rapidEmailSource(f.Store, alias)
	assert.Error(t, err, "archive aliases are not canonical rapid selectors")
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: old.ID, IntoSourceID: current.ID})
	require.NoError(t, err)
	_, err = rapidEmailSource(f.Store, old.Identifier)
	assert.Error(t, err, "retired mail sources cannot fall back to credentials or creation")
}

func TestRapidEmailResolverSelectsCurrentSourceAfterReplacement(t *testing.T) {
	f := storetest.New(t)
	old, err := f.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(t, err)
	_, err = f.Store.GetOrCreateSource("gmail", "anchor@example.test")
	require.NoError(t, err)
	first, err := rapidEmailSource(f.Store, old.Identifier)
	require.NoError(t, err)
	assert.Equal(t, old.ID, first.ID)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM sources WHERE id=?`), old.ID)
	require.NoError(t, err)
	replacement, err := f.Store.GetOrCreateSource("gmail", old.Identifier)
	require.NoError(t, err)
	require.NotEqual(t, old.ID, replacement.ID)
	current, err := rapidEmailSource(f.Store, old.Identifier)
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, current.ID, "each valid pass resolves current Store state")
}

func TestRapidEmailRecordsSelectedSourceFailureTelemetry(t *testing.T) {
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}, Accounts: []config.AccountSchedule{{Email: src.Identifier, Schedule: "0 * * * *", Enabled: true, PollInterval: 30 * time.Second}}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	var events []map[string]any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal(t, telemetry.EventSyncFailed, event)
		events = append(events, props)
		return nil
	}}
	credentials := 0
	for range 2 {
		err := runScheduledSync(ctx, src.Identifier, f.Store, func(context.Context, string) (*oauth.Manager, error) {
			credentials++
			return nil, errors.New("synthetic-private-diagnostic")
		}, state)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "synthetic-private-diagnostic")
	}
	assert.Equal(t, 2, credentials)
	require.Len(t, events, 1, "consecutive selected-source failures keep existing telemetry deduplication")
	assert.Equal(t, map[string]any{"sync_source": "gmail", "reason": "other"}, events[0], "only existing sanitized fields are recorded")
}

func TestRapidEmailCooperativeYieldDoesNotRecordFailureTelemetry(t *testing.T) {
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(t, err)
	dir := t.TempDir()
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}, Accounts: []config.AccountSchedule{{Email: src.Identifier, Schedule: "0 * * * *", Enabled: true, PollInterval: 30 * time.Second}}}
	ctx, requestPreemption := jobctx.WithPreemption(t.Context())
	ctx = testInvocationContext(ctx, cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	events := 0
	state.syncFailures = &telemetry.SyncFailures{Capture: func(string, map[string]any) error { events++; return nil }}
	cause := errors.New("synthetic source failure during cooperative yield")
	err = runScheduledSync(ctx, src.Identifier, f.Store, func(context.Context, string) (*oauth.Manager, error) {
		requestPreemption()
		return nil, cause
	}, state)
	var acquisition *scheduler.AcquisitionError
	require.ErrorAs(t, err, &acquisition)
	assert.Equal(t, scheduler.AcquisitionCancelled, acquisition.Outcome)
	assert.ErrorIs(t, err, cause, "suppressing telemetry must preserve the actual returned cause")
	assert.NoError(t, ctx.Err(), "cooperative preemption does not cancel its context")
	assert.True(t, jobctx.PreemptionRequested(ctx))
	assert.Zero(t, events, "an interrupted acquisition keeps ordinary cooperative-yield telemetry semantics")
}
