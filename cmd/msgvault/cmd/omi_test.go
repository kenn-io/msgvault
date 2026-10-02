package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConfiguredOmiSyncAndCacheRefresh(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "partial error"}[failing], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource(sourceTypeOmi, "work")
			require.NoError(err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("offset") != "0" {
					if failing {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_, _ = w.Write([]byte("[]"))
					return
				}
				_, _ = w.Write([]byte(`[{"id":"meeting-1","structured":{"title":"Synthetic meeting","overview":"Synthetic summary","action_items":[]},"transcript_segments":[]}]`))
			}))
			defer server.Close()
			refreshes := 0
			original := rebuildOmiCacheAfterScheduledSync
			rebuildOmiCacheAfterScheduledSync = func(ctx context.Context, job string) error {
				refreshes++
				require.NoError(ctx.Err())
				assert.Equal("omi:work", job)
				return nil
			}
			defer func() { rebuildOmiCacheAfterScheduledSync = original }()

			err = runConfiguredOmiSync(context.Background(), st, config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL})
			if failing {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, refreshes)
		})
	}
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	err := runConfiguredOmiSync(context.Background(), st, config.OmiSource{Identifier: "missing", AccountEmail: "owner@example.com", APIKey: "test"})
	require.ErrorContains(err, "add-omi missing")
	sources, err := st.ListSources(sourceTypeOmi)
	require.NoError(err)
	assert.Empty(sources)
}

func TestManualOmiUsesInvocationConfiguration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	markDaemonCLISubprocessForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("Bearer omi_dev_synthetic", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`[{"id":"meeting-1","structured":{"title":"Synthetic meeting","overview":"Synthetic summary","action_items":[]},"transcript_segments":[]}]`))
	}))
	t.Cleanup(server.Close)
	oldLimit, oldAfter, oldFull := syncOmiLimit, syncOmiAfter, syncOmiFull
	syncOmiLimit, syncOmiAfter, syncOmiFull = 1, "", false
	t.Cleanup(func() { syncOmiLimit, syncOmiAfter, syncOmiFull = oldLimit, oldAfter, oldFull })
	savedRefresh := rebuildOmiCacheAfterWrite
	t.Cleanup(func() { rebuildOmiCacheAfterWrite = savedRefresh })
	for _, identifier := range []string{"work", "personal"} {
		cfg := lifecycleTestConfig(t.TempDir())
		cfg.Omi = []config.OmiSource{{Identifier: identifier, AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL}}
		ctx := withStoreResolverConfig(t, cfg)
		refreshes := 0
		// Pin the cache boundary's invocation argument; the HTTP request and
		// archive writes run through production command and importer code.
		rebuildOmiCacheAfterWrite = func(dbPath string, state *invocation) error {
			refreshes++
			assert.Same(cfg, state.cfg)
			assert.Equal(cfg.DatabaseDSN(), dbPath)
			return nil
		}
		for _, operation := range []*cobra.Command{addOmiCmd, syncOmiCmd} {
			command := &cobra.Command{Use: operation.Use}
			command.SetContext(ctx)
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			require.NoError(operation.RunE(command, []string{identifier}))
		}
		assert.Equal(1, refreshes)
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		sources, err := st.ListSources(sourceTypeOmi)
		require.NoError(err)
		require.Len(sources, 1)
		assert.Equal(identifier, sources[0].Identifier)
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, sources[0].ID).Scan(&count))
		assert.Equal(1, count)
		require.NoError(st.Close())
	}
}

func TestScheduledOmiRequestsCacheRefreshWithDaemonInvocation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// This queue is SQLite-only; PostgreSQL intentionally skips analytics builds.
	cfg := lifecycleTestConfig(t.TempDir())
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource(sourceTypeOmi, "work")
	require.NoError(err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "0" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"scheduled-meeting","structured":{"title":"Synthetic meeting","overview":"Synthetic summary","action_items":[]},"transcript_segments":[]}]`))
	}))
	t.Cleanup(server.Close)
	cfg.Analytics.AutoBuildCache = true
	state := testInvocationWithConfig(cfg)
	requested := make(chan string, 1)
	// Observe the real post-sync helper and background request queue, without
	// launching the separate DuckDB builder process in this scheduler test.
	refresher := newBackgroundCacheRefresher(withInvocation(context.Background(), state), func(_ context.Context, identifier string) error {
		requested <- identifier
		return nil
	}, nil)
	oldRefresher := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		require.NoError(shutdownBackgroundCacheRefresher(refresher))
		daemonCacheRefresher = oldRefresher
	})
	sched := scheduler.New(nil).WithLogger(testDiscardLogger())
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(registerScheduledOmiJob(sched, state, st, config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL, Enabled: true, Schedule: "0 */6 * * *"}))
	require.NoError(sched.TriggerJob("omi:work"))
	select {
	case identifier := <-requested:
		assert.Equal("omi:work", identifier)
	case <-time.After(10 * time.Second):
		require.FailNow("scheduled Omi sync did not request a cache refresh")
	}
	statuses := sched.JobStatus()
	require.Len(statuses, 1)
	assert.Empty(statuses[0].LastError)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, source.ID).Scan(&count))
	assert.Equal(1, count)
}

func TestDaemonManualOmiQueuesCacheRefreshAfterRealImport(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	oldLimit, oldAfter, oldFull := syncOmiLimit, syncOmiAfter, syncOmiFull
	syncOmiLimit, syncOmiAfter, syncOmiFull = 1, "", false
	t.Cleanup(func() { syncOmiLimit, syncOmiAfter, syncOmiFull = oldLimit, oldAfter, oldFull })
	for _, tc := range []struct {
		name      string
		auto      bool
		flag      string
		wantQueue bool
		wantMode  buildCacheMode
	}{
		{name: "automatic", auto: true, wantQueue: true, wantMode: buildCacheModeScheduledAuto},
		{name: "forced", flag: "--build-cache", wantQueue: true, wantMode: buildCacheModeAuto},
		{name: "skipped", auto: true, flag: "--no-build-cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Analytics.AutoBuildCache = tc.auto
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			_, err = st.GetOrCreateSource(sourceTypeOmi, "work")
			require.NoError(err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`[{"id":"manual-meeting","structured":{"title":"Synthetic meeting","overview":"Synthetic summary","action_items":[]},"transcript_segments":[]}]`))
			}))
			t.Cleanup(server.Close)
			cfg.Omi = []config.OmiSource{{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL}}
			testCtx := withStoreResolverConfig(t, cfg)
			queued := make(chan buildCacheMode, 1)
			jobs := newCacheBuildJobs(testCtx, nil, func(_ context.Context, mode buildCacheMode) error {
				queued <- mode
				return nil
			})
			t.Cleanup(func() {
				waitCtx, cancel := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
				defer cancel()
				require.True(jobs.waitContext(waitCtx))
			})
			for _, name := range []string{"build-cache", "no-build-cache"} {
				if flag := syncOmiCmd.Flags().Lookup(name); flag != nil {
					require.NoError(flag.Value.Set(flag.DefValue))
					flag.Changed = false
					t.Cleanup(func() { require.NoError(flag.Value.Set(flag.DefValue)); flag.Changed = false })
				}
			}
			adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs}
			args := []string{"sync-omi", "work"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			// Run the production child command in process at the subprocess boundary;
			// HTTP ingestion and the parent's detached cache queue remain real.
			err = adapter.runCLICommandWithRunner(testCtx, api.CLIRunRequest{Args: args}, nil,
				func(ctx context.Context, args []string, _ map[string]string, _ string, _ func(string, string) error) error {
					command := &cobra.Command{Use: syncOmiCmd.Use, Args: syncOmiCmd.Args, RunE: syncOmiCmd.RunE}
					command.Flags().AddFlagSet(syncOmiCmd.Flags())
					command.SetArgs(args[1:])
					command.SetOut(&bytes.Buffer{})
					command.SetErr(&bytes.Buffer{})
					return command.ExecuteContext(ctx)
				})
			require.NoError(err)
			waitCtx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
			defer cancel()
			require.True(jobs.waitContext(waitCtx))
			var gotMode buildCacheMode
			var gotQueue bool
			select {
			case gotMode = <-queued:
				gotQueue = true
			default:
			}
			assert.Equal(tc.wantQueue, gotQueue)
			if tc.wantQueue {
				assert.Equal(tc.wantMode, gotMode)
			}
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Equal(1, count)
		})
	}
}
