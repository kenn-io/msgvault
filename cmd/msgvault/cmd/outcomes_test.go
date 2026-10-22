package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestOmiOutageOutlastingBudget(t *testing.T) {
	testutil.SkipIfPostgres(t, "synctest bubble can't own PostgreSQL connection timers")
	st := storetest.New(t)
	_, err := st.Store.GetOrCreateSource("omi", "work")
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		transport := http.DefaultTransport
		http.DefaultTransport = server.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = transport })
		ctx := testInvocationContext(t.Context(), lifecycleTestConfig(t.TempDir()), invocationOptions{})
		var events []map[string]any
		invocationFromContext(ctx).syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
			assert.Equal(t, "sync_failed", event)
			events = append(events, props)
			return nil
		}}
		ctx, cancel := context.WithTimeoutCause(ctx, 5*time.Minute, jobctx.ErrRunBudgetExceeded)
		defer cancel()
		err := runConfiguredOmiSync(ctx, st.Store, t.TempDir(), config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "synthetic", BaseURL: server.URL})
		require.ErrorContains(err, "503")
		require.ErrorIs(context.Cause(ctx), jobctx.ErrRunBudgetExceeded)
		require.Len(events, 1)
		assert.Equal(t, "omi", events[0]["sync_source"])
		assert.Equal(t, "other", events[0]["reason"])
	})
}

func TestSourceSetupFailuresAreRecorded(t *testing.T) {
	for _, tt := range []struct {
		sourceType string
		identifier string
		wantError  string
		run        func(context.Context, *store.Store) error
	}{
		{"granola", "work", "has no api_key", func(ctx context.Context, st *store.Store) error {
			return runConfiguredGranolaSync(ctx, st, config.GranolaSource{Identifier: "work"})
		}},
		{"notion_meetings", "work", "has no token", func(ctx context.Context, st *store.Store) error {
			return runConfiguredNotionMeetingsSync(ctx, st, config.NotionMeetingsSource{Identifier: "work"})
		}},
		{"omi", "work", "has no api_key", func(ctx context.Context, st *store.Store) error {
			return runConfiguredOmiSync(ctx, st, t.TempDir(), config.OmiSource{Identifier: "work"})
		}},
		{"twenty", "work", "api_key", func(ctx context.Context, st *store.Store) error {
			return runConfiguredTwentySync(ctx, st, config.TwentySource{Identifier: "work", AccountEmail: "owner@example.com", BaseURL: "https://api.example.com"})
		}},
		{"twilio", "work", "account_sid", func(ctx context.Context, st *store.Store) error {
			return runConfiguredTwilioSync(ctx, st, config.TwilioSource{Identifier: "work"})
		}},
		{"plaud", "work", "owner differs", func(ctx context.Context, st *store.Store) error {
			return runConfiguredPlaudSync(ctx, st, config.PlaudSource{Identifier: "work", AccountEmail: "changed@example.com"})
		}},
		{"synctech_sms", "+12025550123", "path is required", func(ctx context.Context, st *store.Store) error {
			return runConfiguredSynctechSMSSourceWithStore(ctx, st, config.SynctechSMSSource{Name: "work", OwnerPhone: "+12025550123"})
		}},
	} {
		t.Run(tt.sourceType, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			var events []map[string]any
			invocationFromContext(ctx).syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
				assert.Equal("sync_failed", event)
				events = append(events, props)
				return nil
			}}
			_, err := st.Store.GetOrCreateSource(tt.sourceType, tt.identifier)
			require.NoError(err)
			if tt.sourceType == "plaud" {
				_, err = registerPlaudAccount(st.Store, tt.identifier, "owner@example.com", "owner@example.com")
				require.NoError(err)
			}
			require.ErrorContains(tt.run(ctx, st.Store), tt.wantError)
			require.Len(events, 1)
			assert.Equal(tt.sourceType, events[0]["sync_source"])
		})
	}
}

func TestScheduledGmailTokenFirstFailureOutcome(t *testing.T) {
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
	oauthFailure := func(context.Context, string) (*oauth.Manager, error) {
		return nil, errors.New("synthetic OAuth failure")
	}
	require.Error(runScheduledSync(ctx, "gmail@example.com", st.Store, oauthFailure, state))
	assert.Equal([]map[string]any{{"sync_source": "gmail", "reason": "other"}}, events)
}
