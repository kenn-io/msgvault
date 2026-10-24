package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncPreflightUsesActiveAccountsAndAliases(t *testing.T) {
	for _, tc := range []struct {
		name, selector string
		wantAuth       int
	}{
		{name: "all accounts", wantAuth: 1},
		{name: "alias", selector: "WORK", wantAuth: 1},
		{name: "retired account", selector: "old@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/cli/accounts", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"accounts":[
					{"id":1,"type":"gmail","email":"old@example.com","oauth_app":"old","merged_into_source_id":2},
					{"id":2,"type":"gmail","email":"active@example.com","alias":"work","oauth_app":"active"}
				]}`)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			active, retired := newExpiredPreflightManager(), newExpiredPreflightManager()
			p := buildSyncPreflight(client, HTTPStoreInfo{Kind: HTTPStoreLocalDaemon}, nil)
			p.Interactive, p.OAuthConfigured = true, true
			p.ManagerFor = basePreflight(map[string]*mockReauthorizer{"active": active, "old": retired}).ManagerFor
			require.NoError(preflightReauth(t.Context(), p, tc.selector, 0))
			assert.Equal(tc.wantAuth, active.authorizeCount)
			assert.Zero(retired.authorizeCount, "retired accounts must not open browser authorization")
		})
	}
}

func retireScheduledTestSource(t *testing.T, st *store.Store, sourceType, from, into string) {
	t.Helper()
	require := require.New(t)
	history, err := st.GetOrCreateSource(sourceType, from)
	require.NoError(err)
	live, err := st.GetOrCreateSource(sourceType, into)
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: live.ID})
	require.NoError(err)
}

func TestScheduledOmiSkipsRetiredSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	retireScheduledTestSource(t, st, omi.SourceType, "history", "active")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, "[]")
	}))
	t.Cleanup(server.Close)
	src := config.OmiSource{Identifier: "history", AccountEmail: "owner@example.com", APIKey: "synthetic", BaseURL: server.URL}
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	require.NoError(runConfiguredOmiSync(ctx, st, t.TempDir(), src))
	_, err := omi.NewImporter(st, omi.NewPacedClient(server.URL, src.APIKey, t.TempDir())).Import(ctx,
		omi.ImportOptions{Identifier: src.Identifier, AccountEmail: src.AccountEmail})
	require.ErrorIs(err, store.ErrSourceRetired, "explicit imports must still report retirement")
	assert.Zero(requests)
}

func TestScheduledSynctechSMSSkipsRetiredSource(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	retireScheduledTestSource(t, st, synctechsms.SourceType, "+15550000001", "+15550000002")
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	src := config.SynctechSMSSource{Name: "history", OwnerPhone: "+15550000001", Backend: localValue, Path: t.TempDir()}
	require.NoError(runScheduledSynctechSMSSource(ctx, st, src))
	require.ErrorIs(runConfiguredSynctechSMSSourceWithStore(ctx, st, src), store.ErrSourceRetired)
}

func TestChatwootSyncSkipsRetiredInboxes(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint("mixed=", mixed), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/accounts/9/inboxes":
					_, _ = fmt.Fprint(w, `{"payload":[{"id":7},{"id":8}]}`)
				case "/api/v1/accounts/9/agents":
					_, _ = fmt.Fprint(w, `[]`)
				case "/api/v1/accounts/9/conversations":
					assert.Equal("8", r.URL.Query().Get("inbox_id"))
					_, _ = fmt.Fprint(w, `{"data":{"payload":[]}}`)
				default:
					assert.Fail("unexpected provider request", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic")
			retireScheduledTestSource(t, st, chatwoot.SourceType,
				chatwoot.SourceIdentifier(server.URL, 9, 7), chatwoot.SourceIdentifier(server.URL, 9, 8))
			src := config.ChatwootSource{Identifier: "support", URL: server.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN", Inboxes: []int64{7}}
			if mixed {
				src.Inboxes = append(src.Inboxes, 8)
			}
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Analytics.AutoBuildCache = false
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			require.NoError(runConfiguredChatwootSync(ctx, st, src))
			if mixed {
				assert.Contains(paths, "/api/v1/accounts/9/conversations")
			} else {
				assert.Empty(paths, "a retired-only profile must skip provider discovery")
			}
			_, err := resolveChatwootSyncInboxes(st, src, []int64{7})
			require.ErrorIs(err, store.ErrSourceRetired, "explicit inbox selections must report retirement")
		})
	}
}

func TestScheduledCalendarFiltersRetiredSelections(t *testing.T) {
	for _, selected := range [][]string{{"old", "active"}, {"old"}} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Analytics.AutoBuildCache = true
			st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
			var sources []*store.Source
			for _, id := range []string{"old", "active"} {
				source, err := st.GetOrCreateSource(sourceTypeCalendar, "user@example.com/"+id)
				require.NoError(err)
				require.NoError(st.UpdateSourceSyncConfig(source.ID, fmt.Sprintf(`{"account_email":"user@example.com","calendar_id":%q,"access_role":"owner"}`, id)))
				sources = append(sources, source)
			}
			_, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: sources[0].ID, IntoSourceID: sources[1].ID})
			require.NoError(err)
			client := gcal.NewMockAPI()
			client.Calendars = []gcal.Calendar{{ID: "old", AccessRole: "owner"}, {ID: "active", AccessRole: "owner"}}
			client.FullSyncToken["active"] = "active-token"
			builds, refreshes := 0, 0
			oldBuild := runScheduledBuildCacheSubprocess
			runScheduledBuildCacheSubprocess = func(context.Context) error { refreshes++; return nil }
			t.Cleanup(func() { runScheduledBuildCacheSubprocess = oldBuild })
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			invocationFromContext(ctx).logger = testDiscardLogger()
			err = runConfiguredGCalSyncWithClientBuilder(ctx, st,
				config.GCalSource{Name: "work", Email: "user@example.com", Calendars: selected},
				func(context.Context, string, string, bool, ...bool) (gcal.API, error) { builds++; return client, nil })
			require.NoError(err)
			assert.Zero(client.ListCalendarsCalls(), "registered active selections must not trigger discovery")
			if len(selected) == 1 {
				assert.Zero(builds, "an empty active selection must skip credentials")
				assert.Zero(client.ListEventsCalls())
				assert.Zero(refreshes)
			} else {
				assert.Equal(1, client.ListEventsCalls())
				assert.Equal(1, refreshes)
				live, err := st.GetSourceByID(sources[1].ID)
				require.NoError(err)
				assert.Equal("active-token", live.SyncCursor.String)
			}
			existing, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, "user@example.com")
			require.NoError(err)
			require.ErrorIs(retiredCalendarSelectionError(existing, selected), store.ErrSourceRetired)
			assert.Equal("old", selected[0], "scheduled filtering must not mutate configuration")
		})
	}
}

func TestScheduledCalendarDiscoversNewSelectionAfterRetirement(t *testing.T) {
	for _, selected := range [][]string{{"new"}, {"old", "new"}} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			retired, err := st.GetOrCreateSource(sourceTypeCalendar, "user@example.com/old")
			require.NoError(err)
			require.NoError(st.UpdateSourceSyncConfig(retired.ID,
				`{"account_email":"user@example.com","calendar_id":"old","access_role":"owner"}`))
			retireScheduledTestSource(t, st, sourceTypeCalendar, retired.Identifier, "archive@example.com/old")
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Analytics.AutoBuildCache = false
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			invocationFromContext(ctx).logger = testDiscardLogger()
			client := gcal.NewMockAPI()
			client.Calendars = []gcal.Calendar{{ID: "old", AccessRole: "owner"}, {ID: "new", AccessRole: "owner"}}
			client.FullSyncToken["new"] = "new-calendar-token"
			client.FullEvents["new"] = [][]gcal.Event{{{
				ID: "new-event", Summary: "Example meeting", Status: "confirmed",
				Start: gcal.EventDateTime{Date: "2026-01-01"},
				End:   gcal.EventDateTime{Date: "2026-01-02"},
			}}}
			err = runConfiguredGCalSyncWithClientBuilder(ctx, st,
				config.GCalSource{Name: "work", Email: "user@example.com", Calendars: selected},
				func(context.Context, string, string, bool, ...bool) (gcal.API, error) { return client, nil })
			require.NoError(err)
			assert.Equal(1, client.ListCalendarsCalls(), "new configured calendars must reach discovery")
			assert.Equal(1, client.ListEventsCalls(), "only the new calendar should sync")
			discovered, err := st.GetSourceByTypeAndIdentifier(sourceTypeCalendar, "user@example.com/new")
			require.NoError(err)
			assert.Equal("new-calendar-token", discovered.SyncCursor.String)
			var subject string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT subject FROM messages WHERE source_id = ?`), discovered.ID).Scan(&subject))
			assert.Equal("Example meeting", subject)
			assert.Contains(selected, "new", "selection must not be consumed by filtering")
			if len(selected) == 2 {
				assert.Equal("old", selected[0], "filtering must not mutate configuration")
			}
		})
	}
}
