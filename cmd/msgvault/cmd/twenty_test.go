package cmd

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/twenty"
)

type twentyCommandSource struct {
	probeErr  error
	listCalls int
	page      *twenty.Page
	listErr   error
	nextErr   error
	cancel    func()
}

func (s *twentyCommandSource) Probe(context.Context) error { return s.probeErr }
func (s *twentyCommandSource) ListRecordings(context.Context, string, int) (*twenty.Page, error) {
	s.listCalls++
	if s.listCalls > 1 && s.nextErr != nil {
		if s.cancel != nil {
			s.cancel()
		}
		return nil, s.nextErr
	}
	return s.page, s.listErr
}
func (s *twentyCommandSource) GetCalendar(context.Context, string) (*twenty.Calendar, error) {
	return nil, errors.New("unexpected calendar lookup")
}
func twentySourceConfig(identifier string) config.TwentySource {
	return config.TwentySource{Identifier: identifier, AccountEmail: "recorder@example.com", BaseURL: "https://api.twenty.com", APIKey: "example-key"}
}

func TestTwentyCommandResolutionAndFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, name := range []string{"add-twenty", "sync-twenty"} {
		cmd, _, err := rootCmd.Find([]string{name})
		require.NoError(err)
		assert.Equal(name, cmd.Name())
	}
	for _, name := range []string{"full", "after", "limit", "probe", "build-cache", "no-build-cache"} {
		assert.NotNil(syncTwentyCmd.Flags().Lookup(name))
	}
	assert.True(manualSyncCLICommand([]string{"sync-twenty"}))
	assert.False(manualSyncCLICommand([]string{"sync-twenty", "--probe=true"}))
	cfg := &config.Config{Twenty: []config.TwentySource{twentySourceConfig("work"), twentySourceConfig("other")}}
	all, err := resolveTwentySources(nil, false, cfg)
	require.NoError(err)
	assert.Len(all, 2)
	one, err := resolveTwentySources([]string{"WORK"}, false, cfg)
	require.NoError(err)
	require.Len(one, 1)
	assert.Equal("work", one[0].Identifier)
	_, err = resolveTwentySources(nil, true, cfg)
	require.Error(err)
	_, err = resolveTwentySources(nil, false, nil)
	require.Error(err)
	_, err = resolveTwentySources([]string{"missing"}, false, cfg)
	require.ErrorContains(err, "configured: work, other")
}
func TestTwentyProbeOnlyReportsCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := &twentyCommandSource{}
	var out bytes.Buffer
	require.NoError(runTwentyProbe(t.Context(), &out, source))
	assert.Contains(out.String(), "Recording, calendar and participant access: available")
	assert.Zero(source.listCalls)
	source.probeErr = errors.New("read access unavailable")
	require.Error(runTwentyProbe(t.Context(), &out, source))
}
func TestTwentyCommandsUseInvocationConfiguration(t *testing.T) {
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := &config.Config{Twenty: []config.TwentySource{twentySourceConfig("work")}}
	for _, command := range []*cobra.Command{addTwentyCmd, syncTwentyCmd} {
		cmd := &cobra.Command{}
		cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
		assert.ErrorContains(command.RunE(cmd, []string{"missing"}), "configured: work")
	}
}
func TestConfiguredTwentySyncRefusesRemovedSource(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	err := runConfiguredTwentySync(t.Context(), st, twentySourceConfig("removed"))
	require.ErrorContains(err, "add-twenty")
}
func TestConfiguredTwentySyncRefreshesWithUncanceledContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("twenty", "work")
	require.NoError(err)
	oldFactory, oldRefresh := newTwentyClient, rebuildTwentyCacheAfterScheduledSync
	t.Cleanup(func() { newTwentyClient = oldFactory; rebuildTwentyCacheAfterScheduledSync = oldRefresh })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &twentyCommandSource{nextErr: context.Canceled, cancel: cancel}
	// The first page commits before discovery fails on the next page.
	source.page = &twenty.Page{HasMore: true, NextCursor: "next", Records: []twenty.Recording{{ID: "r1", CreatedAt: "2026-09-01T10:00:00Z", Raw: jsontext.Value(`{"id":"r1","createdAt":"2026-09-01T10:00:00Z","summary":{"markdown":"Summary"}}`)}}}
	newTwentyClient = func(string, string) (twenty.Source, error) { return source, nil }
	refreshes := 0
	rebuildTwentyCacheAfterScheduledSync = func(ctx context.Context, job string) error {
		require.NoError(ctx.Err())
		assert.Equal("twenty:work", job)
		refreshes++
		return nil
	}
	require.ErrorIs(runConfiguredTwentySync(ctx, st, twentySourceConfig("work")), context.Canceled)
	assert.Equal(1, refreshes)
}
func TestTwentyPartialWritesRefreshOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	writes := &twenty.ImportSummary{MeetingsAdded: 1}
	accumulateTwentyWrites(writes, &twenty.ImportSummary{MeetingsUpdated: 1})
	refreshed := 0
	err := finishTwentyImport("other", writes, context.Canceled, func() error { refreshed++; return nil })
	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, refreshed)
	assert.Equal(int64(1), writes.MeetingsUpdated)
}

func TestServeTwentySyncNowCompletes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.APIPort = freeTCPPort(t)
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Analytics.AutoBuildCache = false
	cfg.Vector.Enabled = false
	sourceConfig := twentySourceConfig("work")
	sourceConfig.Enabled = true
	sourceConfig.Schedule = "0 0 1 1 *"
	cfg.Twenty = []config.TwentySource{sourceConfig}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource("twenty", "work")
	require.NoError(err)
	oldFactory := newTwentyClient
	t.Cleanup(func() { newTwentyClient = oldFactory })
	newTwentyClient = func(string, string) (twenty.Source, error) {
		return &twentyCommandSource{page: &twenty.Page{Records: []twenty.Recording{{ID: "r1", CreatedAt: "2026-09-01T10:00:00Z", Raw: jsontext.Value(`{"id":"r1","summary":{"markdown":"Planning summary"}}`)}}}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(err)
		case <-time.After(serveLifecycleTestTimeout):
			require.FailNow("daemon did not stop")
		}
	})
	waitForServeHealth(t, cfg.Server.APIPort, errCh)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.APIPort)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post(baseURL+"/api/v1/sync/work?source_type=twenty", "application/json", nil)
	require.NoError(err)
	require.NoError(response.Body.Close())
	require.Equal(http.StatusAccepted, response.StatusCode)
	var status api.SourceStatusResponse
	require.Eventually(func() bool {
		response, err := client.Get(baseURL + "/api/v1/sources/status?source_type=twenty")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return json.UnmarshalRead(response.Body, &status) == nil && len(status.Sources) == 1 && status.Sources[0].LastSuccessfulSync != nil && status.Sources[0].CanSync
	}, serveLifecycleTestTimeout, 20*time.Millisecond, "scheduled import did not finish")
	assert.Equal(int64(1), status.Sources[0].LastSuccessfulSync.MessagesAdded)
	assert.Empty(status.Sources[0].SchedulerLastError)
}

func TestTwentyManualMultipleSourcesRefreshesEarlierWritesOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	first, second := twentySourceConfig("first"), twentySourceConfig("second")
	second.APIKey = "second-key"
	cfg.Twenty = []config.TwentySource{first, second}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	oldFactory, oldRefresh := newTwentyClient, rebuildTwentyCacheAfterWrite
	t.Cleanup(func() { newTwentyClient = oldFactory; rebuildTwentyCacheAfterWrite = oldRefresh })
	newTwentyClient = func(_, key string) (twenty.Source, error) {
		if key == "second-key" {
			return &twentyCommandSource{listErr: errors.New("discovery unavailable")}, nil
		}
		return &twentyCommandSource{page: &twenty.Page{Records: []twenty.Recording{{ID: "r1", CreatedAt: "2026-09-01T10:00:00Z", Raw: jsontext.Value(`{"id":"r1","summary":{"markdown":"Summary"}}`)}}}}, nil
	}
	refreshed := 0
	rebuildTwentyCacheAfterWrite = func(string, *invocation) error { refreshed++; return nil }
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(addTwentyCmd.RunE(cmd, []string{"first"}))
	require.NoError(addTwentyCmd.RunE(cmd, []string{"second"}))
	require.ErrorContains(syncTwentyCmd.RunE(cmd, nil), "discovery unavailable")
	assert.Equal(1, refreshed)
	source, err := st.GetSourceByTypeAndIdentifier("twenty", "first")
	require.NoError(err)
	run, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(int64(1), run.MessagesAdded)
	require.NoError(st.RemoveSource(source.ID))
	_, err = st.GetSourceByTypeAndIdentifier("twenty", "first")
	require.ErrorIs(err, store.ErrSourceNotFound)
}

func TestSyncTwentyContinuesAfterSourceImportFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	first, second := twentySourceConfig("first"), twentySourceConfig("second")
	first.APIKey, second.APIKey = "first-key", "second-key"
	cfg.Twenty = []config.TwentySource{first, second}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	for _, source := range cfg.Twenty {
		_, err = st.GetOrCreateSource(twenty.SourceType, source.Identifier)
		require.NoError(err)
	}

	firstClient := &twentyCommandSource{listErr: errors.New("first discovery unavailable")}
	secondClient := &twentyCommandSource{page: &twenty.Page{Records: []twenty.Recording{{ID: "r2", CreatedAt: "2026-09-01T10:00:00Z", Raw: jsontext.Value(`{"id":"r2","createdAt":"2026-09-01T10:00:00Z","summary":{"markdown":"Second source summary"}}`)}}}}
	oldFactory, oldRefresh := newTwentyClient, rebuildTwentyCacheAfterWrite
	t.Cleanup(func() { newTwentyClient, rebuildTwentyCacheAfterWrite = oldFactory, oldRefresh })
	newTwentyClient = func(_, key string) (twenty.Source, error) {
		if key == "first-key" {
			return firstClient, nil
		}
		return secondClient, nil
	}
	refreshes := 0
	rebuildTwentyCacheAfterWrite = func(string, *invocation) error { refreshes++; return nil }
	oldLimit, oldAfter, oldFull, oldProbe := syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe
	t.Cleanup(func() {
		syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe = oldLimit, oldAfter, oldFull, oldProbe
	})
	syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe = 0, "", false, false
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err = syncTwentyCmd.RunE(cmd, nil)

	require.ErrorContains(err, "twenty sync first failed: first discovery unavailable")
	assert.Equal(1, firstClient.listCalls)
	assert.Equal(1, secondClient.listCalls)
	assert.Contains(out.String(), "Syncing Twenty meetings for first")
	assert.Contains(out.String(), "Syncing Twenty meetings for second")
	secondSource, err := st.GetSourceByTypeAndIdentifier(twenty.SourceType, "second")
	require.NoError(err)
	run, err := st.GetLatestSync(secondSource.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, run.Status)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Equal(1, refreshes)
}

func TestSyncTwentyStopsAfterCanceledSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	first, second := twentySourceConfig("first"), twentySourceConfig("second")
	first.APIKey, second.APIKey = "first-key", "second-key"
	cfg.Twenty = []config.TwentySource{first, second}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	for _, source := range cfg.Twenty {
		_, err = st.GetOrCreateSource(twenty.SourceType, source.Identifier)
		require.NoError(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	firstClient := &twentyCommandSource{
		page:    &twenty.Page{HasMore: true, NextCursor: "next", Records: []twenty.Recording{{ID: "r1", CreatedAt: "2026-09-01T10:00:00Z", Raw: jsontext.Value(`{"id":"r1","createdAt":"2026-09-01T10:00:00Z","summary":{"markdown":"Committed before cancellation"}}`)}}},
		nextErr: context.Canceled,
		cancel:  cancel,
	}
	secondClient := &twentyCommandSource{page: &twenty.Page{}}
	oldFactory, oldRefresh := newTwentyClient, rebuildTwentyCacheAfterWrite
	t.Cleanup(func() { newTwentyClient, rebuildTwentyCacheAfterWrite = oldFactory, oldRefresh })
	newTwentyClient = func(_, key string) (twenty.Source, error) {
		if key == "first-key" {
			return firstClient, nil
		}
		return secondClient, nil
	}
	refreshes := 0
	rebuildTwentyCacheAfterWrite = func(string, *invocation) error { refreshes++; return nil }
	oldLimit, oldAfter, oldFull, oldProbe := syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe
	t.Cleanup(func() {
		syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe = oldLimit, oldAfter, oldFull, oldProbe
	})
	syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe = 0, "", false, false
	cmd := &cobra.Command{}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	err = syncTwentyCmd.RunE(cmd, nil)

	require.ErrorIs(err, context.Canceled)
	assert.Equal(2, firstClient.listCalls)
	assert.Zero(secondClient.listCalls)
	assert.Equal(1, refreshes)
	firstSource, err := st.GetSourceByTypeAndIdentifier(twenty.SourceType, "first")
	require.NoError(err)
	run, err := st.GetLatestSync(firstSource.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.Equal(int64(1), run.MessagesAdded)
	secondSource, err := st.GetSourceByTypeAndIdentifier(twenty.SourceType, "second")
	require.NoError(err)
	_, err = st.GetLatestSync(secondSource.ID)
	require.Error(err, "a canceled command must not start the next source")
}

func TestSyncTwentyContinuesWhenOneSourceHasNoAPIKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	unconfigured := twentySourceConfig("unconfigured")
	unconfigured.APIKey = ""
	ready := twentySourceConfig("ready")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)
	}))
	defer srv.Close()
	ready.BaseURL = srv.URL
	cfg.Twenty = []config.TwentySource{unconfigured, ready}

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	for _, source := range cfg.Twenty {
		_, err := st.GetOrCreateSource(twenty.SourceType, source.Identifier)
		require.NoError(err)
	}
	oldRefresh := rebuildTwentyCacheAfterWrite
	t.Cleanup(func() { rebuildTwentyCacheAfterWrite = oldRefresh })
	refreshed := 0
	rebuildTwentyCacheAfterWrite = func(string, *invocation) error { refreshed++; return nil }
	oldLimit, oldAfter, oldFull, oldProbe := syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe
	t.Cleanup(func() {
		syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe = oldLimit, oldAfter, oldFull, oldProbe
	})
	syncTwentyLimit, syncTwentyAfter, syncTwentyFull, syncTwentyProbe = 0, "", false, false
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err = syncTwentyCmd.RunE(cmd, nil)

	readySource, lookupErr := st.GetSourceByTypeAndIdentifier(twenty.SourceType, "ready")
	require.NoError(lookupErr)
	run, runErr := st.GetLatestSync(readySource.ID)
	require.NoError(runErr, "the later configured source must still be attempted")
	assert.Equal(store.SyncStatusCompleted, run.Status)
	require.ErrorContains(err, `source "unconfigured"`)
	assert.Contains(out.String(), "Syncing Twenty meetings for ready")
	assert.Equal(1, refreshed)
}
