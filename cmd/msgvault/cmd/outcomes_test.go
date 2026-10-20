package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCalendarPreLoopFailureRecordsOnlySelectedSources(t *testing.T) {
	for _, tc := range []struct {
		name                string
		calendars           []string
		discovery           bool
		want, afterRecovery int
	}{
		{name: "default selection", want: 2, afterRecovery: 3},
		{name: "selected calendar", calendars: []string{"first"}, want: 1, afterRecovery: 2},
		{name: "selected reader", calendars: []string{"reader"}, want: 1, afterRecovery: 1},
		{name: "no registered selection", calendars: []string{"new"}},
		{name: "discovery failure", calendars: []string{"first", "new"}, discovery: true, want: 1, afterRecovery: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t)
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "client_secret.json")
			require.NoError(os.WriteFile(cfg.OAuth.ClientSecrets, []byte(fakeClientSecrets), 0600))
			state := testInvocationWithConfig(cfg)
			ctx := withInvocation(t.Context(), state)
			for _, calendar := range []struct{ email, id, role string }{
				{"user@example.com", "first", "owner"},
				{"user@example.com", "second", "writer"},
				{"user@example.com", "reader", "reader"},
				{"other@example.com", "first", "owner"},
			} {
				source, err := st.Store.GetOrCreateSource("gcal", calendar.email+"/"+calendar.id)
				require.NoError(err)
				require.NoError(st.Store.UpdateSourceSyncConfig(source.ID, fmt.Sprintf(
					`{"account_email":%q,"calendar_id":%q,"access_role":%q}`, calendar.email, calendar.id, calendar.role)))
			}
			events := 0
			state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
				assert.Equal("sync_failed", event)
				assert.Equal(map[string]any{"sync_source": "gcal", "reason": "other"}, props)
				events++
				return nil
			}}
			buildClient := buildCalendarClient
			wantErr := calcontrol.ErrDenied
			if tc.discovery {
				wantErr = errors.New("calendar discovery unavailable")
				client := gcal.NewMockAPI()
				client.ListCalendarsErr = wantErr
				buildClient = func(context.Context, string, string, bool, ...bool) (gcal.API, error) { return client, nil }
			}

			for run := range 3 {
				if run == 2 {
					state.syncFailures.Record("gcal", "user@example.com/first", nil)
				}

				err := runConfiguredGCalSyncWithClientBuilder(ctx, st.Store,
					config.GCalSource{Email: "user@example.com", Calendars: tc.calendars}, buildClient)
				require.ErrorIs(err, wantErr)

				if run < 2 {
					assert.Equal(tc.want, events, "repeat failures do not start new outages")
				} else {
					assert.Equal(tc.afterRecovery, events, "recovery clears only the selected calendar's outage")
				}
			}
		})
	}
}

func TestScheduledSourceOutcomesCountEachType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	var events []map[string]any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal("sync_failed", event)
		events = append(events, props)
		return nil
	}}
	_, err := st.Store.GetOrCreateSource("gmail", "synthetic@example.com")
	require.NoError(err)
	_, err = st.Store.GetOrCreateSource("imap", "synthetic@example.com")
	require.NoError(err)
	oauthFailure := func(context.Context, string) (*oauth.Manager, error) {
		return nil, errors.New("synthetic OAuth failure")
	}
	require.Error(runScheduledSync(ctx, "synthetic@example.com", st.Store, oauthFailure, state))
	assert.Equal([]map[string]any{{"sync_source": "gmail", "reason": "other"}, {"sync_source": "imap", "reason": "other"}}, events)
}
