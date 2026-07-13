package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newGranolaSyncTestServer(t *testing.T, includeSuccessfulNote, includeMissingNote bool) *httptest.Server {
	t.Helper()
	note, err := json.Marshal(map[string]any{
		"id":           "note-success",
		"title":        "Partial import success",
		"owner":        map[string]any{"name": "Test User", "email": "user-a@example.com"},
		"created_at":   "2026-07-12T10:00:00Z",
		"updated_at":   "2026-07-12T10:30:00Z",
		"summary_text": "This note must be persisted before the partial error is returned.",
	})
	require.NoError(t, err)
	var notes []json.RawMessage
	if includeSuccessfulNote {
		notes = append(notes, note)
	}
	if includeMissingNote {
		notes = append(notes, json.RawMessage(`{"id":"note-missing","updated_at":"2026-07-12T11:00:00Z"}`))
	}
	listResponse, err := json.Marshal(map[string]any{
		"notes": notes, "hasMore": false, "cursor": "",
	})
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/notes":
			_, _ = w.Write(listResponse)
		case "/v1/notes/note-success":
			_, _ = w.Write(note)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func installGranolaClientFactory(t *testing.T, baseURL string) {
	t.Helper()
	savedFactory := newGranolaClient
	newGranolaClient = func(_ string, apiKey string) *granola.Client {
		return granola.NewClient(baseURL, apiKey)
	}
	t.Cleanup(func() { newGranolaClient = savedFactory })
}

func TestAddGranolaIdentityConfirmsPrimaryWhenAliasExists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeGranola, "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "user-b@example.com", "manual"))
	var out bytes.Buffer

	registered, err := registerMeetingSource(&out, st, sourceTypeGranola, "work", " User-Z@Example.COM ")

	require.NoError(err)
	assert.Equal(source.ID, registered.ID)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 2)
	assert.Equal("user-b@example.com", identities[0].Address)
	assert.Equal("user-z@example.com", identities[1].Address)
	assert.Contains(out.String(), "Confirmed identity user-z@example.com")
	assert.Contains(out.String(), "sync-granola work --full")
	assert.Nil(addGranolaCmd.Flags().Lookup("no-default-identity"), "meeting identity confirmation is mandatory")
}

func TestConfiguredGranolaMissingRegisteredSourceStopsBeforeClient(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	constructed := 0
	savedFactory := newGranolaClient
	newGranolaClient = func(baseURL, apiKey string) *granola.Client {
		constructed++
		return granola.NewClient(baseURL, apiKey)
	}
	t.Cleanup(func() { newGranolaClient = savedFactory })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runConfiguredGranolaSync(ctx, st, config.GranolaSource{
		Identifier:   "work",
		AccountEmail: "user-a@example.com",
		APIKey:       "grn_test",
	})

	require.Error(err)
	assert.Contains(err.Error(), "run msgvault add-granola work")
	assert.Zero(constructed, "missing registration must be rejected before client construction")
	sources, listErr := st.ListSources(granola.SourceType)
	require.NoError(listErr)
	assert.Empty(sources, "scheduled sync must not create an unregistered source")
}

func TestManualGranolaPartialImportRefreshesCacheBeforeReturningError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := newGranolaSyncTestServer(t, true, true)
	installGranolaClientFactory(t, server.URL)
	markDaemonCLISubprocessForTest(t)
	testCfg := lifecycleTestConfig(dataDir)
	testCfg.Granola = []config.GranolaSource{{
		Identifier:   "work",
		AccountEmail: "user-a@example.com",
		APIKey:       "grn_test",
	}}
	withStoreResolverConfig(t, testCfg)

	savedRefresh := rebuildGranolaCacheAfterWrite
	refreshes := 0
	refreshSawWrite := false
	rebuildGranolaCacheAfterWrite = func(dbPath string) {
		refreshes++
		st, openErr := store.Open(dbPath)
		require.NoError(openErr)
		defer func() { require.NoError(st.Close()) }()
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
		refreshSawWrite = count == 1
	}
	t.Cleanup(func() { rebuildGranolaCacheAfterWrite = savedRefresh })

	oldLimit, oldAfter, oldFull := syncGranolaLimit, syncGranolaAfter, syncGranolaFull
	syncGranolaLimit, syncGranolaAfter, syncGranolaFull = 0, "", false
	t.Cleanup(func() {
		syncGranolaLimit, syncGranolaAfter, syncGranolaFull = oldLimit, oldAfter, oldFull
	})
	cmd := &cobra.Command{Use: "sync-granola"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := syncGranolaCmd.RunE(cmd, []string{"work"})

	require.Error(err)
	assert.Contains(err.Error(), "partial Granola sync")
	assert.Equal(1, refreshes)
	assert.True(refreshSawWrite, "cache refresh must run after the successful partial write")
}

func TestManualGranolaCancellationReturnsError(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	server := newGranolaSyncTestServer(t, true, false)
	installGranolaClientFactory(t, server.URL)
	markDaemonCLISubprocessForTest(t)
	testCfg := lifecycleTestConfig(dataDir)
	testCfg.Granola = []config.GranolaSource{{
		Identifier: "work", AccountEmail: "user-a@example.com", APIKey: "grn_test",
	}}
	withStoreResolverConfig(t, testCfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{Use: "sync-granola"}
	cmd.SetContext(ctx)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := syncGranolaCmd.RunE(cmd, []string{"work"})

	require.ErrorIs(err, context.Canceled)
}

func TestConfiguredGranolaPartialImportRefreshesCacheBeforeReturningError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := newGranolaSyncTestServer(t, true, true)
	installGranolaClientFactory(t, server.URL)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(granola.SourceType, "work")
	require.NoError(err)

	savedRefresh := rebuildGranolaCacheAfterScheduledSync
	refreshes := 0
	refreshSawWrite := false
	ctx, cancel := context.WithCancel(context.Background())
	var refreshContextErr error
	rebuildGranolaCacheAfterScheduledSync = func(refreshCtx context.Context, _ string) {
		refreshes++
		cancel()
		refreshContextErr = refreshCtx.Err()
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
		refreshSawWrite = count == 1
	}
	t.Cleanup(func() { rebuildGranolaCacheAfterScheduledSync = savedRefresh })

	err = runConfiguredGranolaSync(ctx, st, config.GranolaSource{
		Identifier:   "work",
		AccountEmail: "user-a@example.com",
		APIKey:       "grn_test",
	})

	require.Error(err)
	assert.Contains(err.Error(), "partial Granola sync")
	assert.Equal(1, refreshes)
	assert.True(refreshSawWrite, "cache refresh must run after the successful partial write")
	require.ErrorIs(ctx.Err(), context.Canceled)
	assert.NoError(refreshContextErr, "cache refresh must be detached from the canceled scheduled-sync context")
}

func TestConfiguredGranolaPartialImportWithoutWritesSkipsCacheRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := newGranolaSyncTestServer(t, false, true)
	installGranolaClientFactory(t, server.URL)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(granola.SourceType, "work")
	require.NoError(err)

	savedRefresh := rebuildGranolaCacheAfterScheduledSync
	refreshes := 0
	rebuildGranolaCacheAfterScheduledSync = func(context.Context, string) { refreshes++ }
	t.Cleanup(func() { rebuildGranolaCacheAfterScheduledSync = savedRefresh })

	err = runConfiguredGranolaSync(context.Background(), st, config.GranolaSource{
		Identifier:   "work",
		AccountEmail: "user-a@example.com",
		APIKey:       "grn_test",
	})

	require.Error(err)
	assert.Contains(err.Error(), "partial Granola sync")
	assert.Zero(refreshes, "a partial import with no successful writes must not refresh the cache")
}

func TestManualGranolaLaterFailureRefreshesEarlierSourceWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	cleanServer := newGranolaSyncTestServer(t, true, false)
	failingServer := newGranolaSyncTestServer(t, false, true)
	savedFactory := newGranolaClient
	newGranolaClient = func(_ string, apiKey string) *granola.Client {
		if apiKey == "grn_clean" {
			return granola.NewClient(cleanServer.URL, apiKey)
		}
		return granola.NewClient(failingServer.URL, apiKey)
	}
	t.Cleanup(func() { newGranolaClient = savedFactory })
	markDaemonCLISubprocessForTest(t)
	testCfg := lifecycleTestConfig(dataDir)
	testCfg.Granola = []config.GranolaSource{
		{Identifier: "first", AccountEmail: "user-a@example.com", APIKey: "grn_clean"},
		{Identifier: "second", AccountEmail: "user-b@example.com", APIKey: "grn_partial"},
	}
	withStoreResolverConfig(t, testCfg)

	savedRefresh := rebuildGranolaCacheAfterWrite
	refreshes := 0
	refreshSawWrite := false
	rebuildGranolaCacheAfterWrite = func(dbPath string) {
		refreshes++
		st, openErr := store.Open(dbPath)
		require.NoError(openErr)
		defer func() { require.NoError(st.Close()) }()
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
		refreshSawWrite = count == 1
	}
	t.Cleanup(func() { rebuildGranolaCacheAfterWrite = savedRefresh })

	oldLimit, oldAfter, oldFull := syncGranolaLimit, syncGranolaAfter, syncGranolaFull
	syncGranolaLimit, syncGranolaAfter, syncGranolaFull = 0, "", false
	t.Cleanup(func() {
		syncGranolaLimit, syncGranolaAfter, syncGranolaFull = oldLimit, oldAfter, oldFull
	})
	cmd := &cobra.Command{Use: "sync-granola"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := syncGranolaCmd.RunE(cmd, nil)

	require.Error(err)
	assert.Contains(err.Error(), "partial Granola sync")
	assert.Equal(1, refreshes)
	assert.True(refreshSawWrite, "the later error must not bypass refresh of the earlier source write")
}

func TestManualGranolaPrevalidatesAllSourcesBeforeImport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := newGranolaSyncTestServer(t, true, false)
	constructed := 0
	savedFactory := newGranolaClient
	newGranolaClient = func(_ string, apiKey string) *granola.Client {
		constructed++
		return granola.NewClient(server.URL, apiKey)
	}
	t.Cleanup(func() { newGranolaClient = savedFactory })
	markDaemonCLISubprocessForTest(t)
	testCfg := lifecycleTestConfig(dataDir)
	testCfg.Granola = []config.GranolaSource{
		{Identifier: "first", AccountEmail: "user-a@example.com", APIKey: "grn_clean"},
		{Identifier: "second", AccountEmail: "user-b@example.com"},
	}
	withStoreResolverConfig(t, testCfg)

	cmd := &cobra.Command{Use: "sync-granola"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := syncGranolaCmd.RunE(cmd, nil)

	require.Error(err)
	assert.Contains(err.Error(), `[[granola]] entry "second" has no api_key`)
	assert.Zero(constructed, "all selected sources must validate before any client or import starts")
	_, statErr := os.Stat(testCfg.DatabaseDSN())
	assert.ErrorIs(statErr, os.ErrNotExist, "invalid configuration must not create or write the archive")
}
