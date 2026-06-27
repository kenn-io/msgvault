package cmd

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarDateBounds(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	t.Run("valid bounds map to RFC3339 UTC", func(t *testing.T) {
		tmin, tmax, err := calendarDateBounds(cmd, "2024-01-15", "2024-12-31")
		require.NoError(t, err)
		assert.Equal(t, "2024-01-15T00:00:00Z", tmin)
		assert.Equal(t, "2024-12-31T00:00:00Z", tmax)
	})

	t.Run("empty bounds yield empty strings", func(t *testing.T) {
		tmin, tmax, err := calendarDateBounds(cmd, "", "")
		require.NoError(t, err)
		assert.Empty(t, tmin)
		assert.Empty(t, tmax)
	})

	t.Run("invalid after is rejected", func(t *testing.T) {
		_, _, err := calendarDateBounds(cmd, "01/15/2024", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--after")
	})

	t.Run("invalid before is rejected", func(t *testing.T) {
		_, _, err := calendarDateBounds(cmd, "", "not-a-date")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--before")
	})
}

func TestCalendarLabel(t *testing.T) {
	assert.Equal(t, "Personal [primary]", calendarLabel(gcal.Calendar{ID: "primary", Summary: "Personal"}))
	assert.Equal(t, "holidays@x", calendarLabel(gcal.Calendar{ID: "holidays@x"}))
}

func TestCalendarSyncUsesFullWhenSelectionCanExpandCalendars(t *testing.T) {
	assert := assert.New(t)
	existing := []*store.Source{calendarSourceWithSyncConfig(`{"calendar_id":"primary"}`)}
	assert.False(calendarSyncShouldRunFullForSources(existing, false, false, "", nil, false),
		"an unfiltered sync with registered calendars can use incremental")
	assert.True(calendarSyncShouldRunFullForSources(existing, false, true, "", nil, false),
		"--all-calendars must enumerate calendars even when one source already exists")
	assert.True(calendarSyncShouldRunFullForSources(existing, false, false, "reader", nil, false),
		"--min-access-role can add calendars not yet registered")
	assert.False(calendarSyncShouldRunFullForSources(existing, false, false, "", []string{"primary"}, false),
		"a requested calendar that is already registered can use incremental")
	assert.True(calendarSyncShouldRunFullForSources(existing, false, false, "", []string{"primary", "shared@example.com"}, false),
		"a configured calendar missing from registered sources needs a full registration sync")
	assert.True(calendarSyncShouldRunFullForSources(existing, false, false, "", []string{"shared@example.com"}, false),
		"--calendar can name a calendar not yet registered")
	assert.True(calendarSyncShouldRunFullForSources(existing, true, false, "", nil, false),
		"--full still forces a full sync")
	assert.True(calendarSyncShouldRunFullForSources(nil, false, false, "", nil, false),
		"first sync must enumerate calendars")
	assert.True(calendarSyncShouldRunFullForSources([]*store.Source{calendarSourceWithSyncConfig(`{"account_email":"user@example.com"}`)}, false, false, "", nil, false),
		"malformed registered calendar sources should self-heal with a full sync")
	assert.True(calendarSyncShouldRunFullForSources(existing, false, false, "", []string{"primary"}, true),
		"--after/--before must force a full sync so bounds are honored")
}

func TestCalendarSyncFullOnlyOptions(t *testing.T) {
	assert := assert.New(t)
	assert.False(calendarSyncHasFullOnlyOptions("", "", 0))
	assert.True(calendarSyncHasFullOnlyOptions("2024-01-01T00:00:00Z", "", 0),
		"--after must force full sync")
	assert.True(calendarSyncHasFullOnlyOptions("", "2024-12-31T00:00:00Z", 0),
		"--before must force full sync")
	assert.True(calendarSyncHasFullOnlyOptions("", "", 10),
		"--limit must force full sync")
}

func TestCalendarAddOAuthScopes(t *testing.T) {
	assert.ElementsMatch(t, oauth.ScopesCalendar, calendarAddOAuthScopes(false),
		"a new calendar-only account should request only Calendar")
	assert.ElementsMatch(t, oauth.ScopesGmailCalendar, calendarAddOAuthScopes(true),
		"an existing Gmail token must preserve Gmail while adding Calendar")
}

func calendarSourceWithSyncConfig(syncConfig string) *store.Source {
	return &store.Source{SyncConfig: sql.NullString{String: syncConfig, Valid: true}}
}

func TestCalendarStoredOAuthAppUsesRegisteredSource(t *testing.T) {
	sources := []*store.Source{
		{OAuthApp: sql.NullString{}},
		{OAuthApp: sql.NullString{String: "personal", Valid: true}},
	}

	assert.Equal(t, "personal", calendarStoredOAuthApp(sources))
}

func TestCalendarAddOAuthAppDecisionInheritsStoredBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := newCalendarDecisionStore(t)
	src, err := st.GetOrCreateSource(sourceTypeCalendar, "user@acme.com/primary")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID,
		`{"account_email":"user@acme.com","calendar_id":"primary"}`))
	require.NoError(st.UpdateSourceOAuthApp(src.ID, sql.NullString{String: "acme", Valid: true}))

	decision, err := calendarAddOAuthAppDecision(st, "user@acme.com", "", false)
	require.NoError(err)

	assert.Equal("acme", decision.OAuthApp)
	assert.False(decision.BindingChanged)
	assert.True(decision.NeedsClientCheck)
}

func TestCalendarAddOAuthAppDecisionDetectsExplicitRebind(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := newCalendarDecisionStore(t)
	src, err := st.GetOrCreateSource(sourceTypeCalendar, "user@acme.com/primary")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID,
		`{"account_email":"user@acme.com","calendar_id":"primary"}`))
	require.NoError(st.UpdateSourceOAuthApp(src.ID, sql.NullString{String: "old-app", Valid: true}))

	decision, err := calendarAddOAuthAppDecision(st, "user@acme.com", "new-app", true)
	require.NoError(err)

	assert.Equal("new-app", decision.OAuthApp)
	assert.True(decision.BindingChanged)
	assert.True(decision.NeedsClientCheck)
}

func TestCalendarAddTokenReusableRejectsMismatchedInheritedClient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmpDir := t.TempDir()
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600))
	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700))
	writeCalendarToken(t, tokensDir, "user@acme.com", "wrong-client.apps.googleusercontent.com")

	savedCfg, savedLogger := cfg, logger
	defer func() { cfg, logger = savedCfg, savedLogger }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"acme": {ClientSecrets: secretsPath},
			},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	mgr, err := newCalendarOAuthManager(secretsPath, "user@acme.com")
	require.NoError(err)
	decision := calendarAddOAuthApp{OAuthApp: "acme", NeedsClientCheck: true}

	assert.False(calendarAddTokenReusable(mgr, "user@acme.com", decision),
		"a calendar token minted by another OAuth client must force reauthorization")
}

func TestCalendarSyncNextCommandIncludesOAuthApp(t *testing.T) {
	assert.Equal(t,
		"msgvault sync-calendar --oauth-app personal user@example.com",
		calendarSyncNextCommand("user@example.com", "personal"))
	assert.Equal(t,
		"msgvault sync-calendar user@example.com",
		calendarSyncNextCommand("user@example.com", ""))
}

func TestCalendarCommandsRegistered(t *testing.T) {
	have := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		have[c.Name()] = true
	}
	assert.True(t, have["add-calendar"], "add-calendar should be registered")
	assert.True(t, have["sync-calendar"], "sync-calendar should be registered")
}

func newCalendarDecisionStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	return st
}

func writeCalendarToken(t *testing.T, tokensDir, email, clientID string) {
	t.Helper()
	tokenData, err := json.Marshal(map[string]any{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     clientID,
		"scopes":        oauth.ScopesCalendar,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tokensDir, email+".json"), tokenData, 0600))
}
