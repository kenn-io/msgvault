package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScheduledOmiContinuesFullScanForOlderEdits(t *testing.T) {
	unpacedOmiClients(t)
	for _, mode := range []string{"scheduled", "manual", "limit zero", "full false", "cache"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			markDaemonCLISubprocessForTest(t)
			cfg := lifecycleTestConfig(t.TempDir())
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			require.NoError(st.InitSchema())
			src, err := st.GetOrCreateSource(omi.SourceType, "work")
			require.NoError(err)
			changed := false
			var pause context.CancelCauseFunc
			var fullCeiling string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if pause != nil {
					var snippet string
					err := st.DB().QueryRow(`SELECT snippet FROM messages WHERE source_id = ? AND source_message_id = 'recent'`, src.ID).Scan(&snippet)
					if err == nil && strings.Contains(snippet, "Revised recent") {
						pause(jobctx.ErrRunBudgetExceeded)
						<-r.Context().Done()
						return
					}
					fullCeiling = r.URL.Query().Get("end_date")
				}
				offset := r.URL.Query().Get("offset")
				id, created := "recent", "2026-01-10T12:00:00Z"
				if offset == "200" {
					id, created = "older", "2026-01-01T12:00:00Z"
					if mode == "cache" {
						created = "2026-01-10T12:00:00Z"
					}
				} else if offset != "0" {
					_, _ = w.Write([]byte("[]"))
					return
				}
				if lower := r.URL.Query().Get("start_date"); lower != "" && lower > created {
					_, _ = w.Write([]byte("[]"))
					return
				}
				summary, speech := "Original "+id, "Original speech"
				if changed && (id == "recent" || mode != "cache") {
					summary, speech = "Revised "+id, "Revised speech"
				}
				_, _ = fmt.Fprintf(w, `[{"id":%q,"created_at":%q,"structured":{"title":"Synthetic meeting","overview":%q,"action_items":[]},"transcript_segments":[{"text":%q,"start":0,"end":60}]}]`, id, created, summary, speech)
			}))
			t.Cleanup(server.Close)
			imp := omi.NewImporter(st, omi.NewPacedClient(server.URL, "omi_dev_synthetic", t.TempDir()))
			opts := omi.ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			ids, err := st.MessageExistsBatch(src.ID, []string{"recent", "older"})
			require.NoError(err)
			analyticsDir := filepath.Join(cfg.Data.DataDir, "analytics")
			var before query.CacheSyncState
			cachedSnippet := func() string {
				duck, err := sql.Open("duckdb", "")
				require.NoError(err)
				defer func() { require.NoError(duck.Close()) }()
				var snippet string
				require.NoError(duck.QueryRow(`SELECT snippet FROM read_parquet(?, hive_partitioning=true) WHERE source_message_id = 'recent'`, filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&snippet))
				return snippet
			}
			if mode == "cache" {
				_, err = buildCache(cfg.DatabaseDSN(), analyticsDir, false)
				require.NoError(err)
				before, err = query.ReadCacheSyncState(analyticsDir)
				require.NoError(err)
			}
			changed = true
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			pause, opts.Full = cancel, mode != "cache"
			sum, err := imp.Import(ctx, opts)
			require.NoError(err)
			require.ErrorIs(sum.PauseReason, jobctx.ErrRunBudgetExceeded)
			assert.Equal(int64(1), sum.MeetingsUpdated)
			oldRaw, err := st.GetMessageRaw(ids["older"])
			require.NoError(err)
			assert.Contains(string(oldRaw), "Original older")
			pause = nil
			originalRefresh := rebuildOmiCacheAfterScheduledSync
			rebuildOmiCacheAfterScheduledSync = func(context.Context, string) error { return nil }
			t.Cleanup(func() { rebuildOmiCacheAfterScheduledSync = originalRefresh })
			source := config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL}
			switch mode {
			case "cache":
				var pausedUpdates int64
				require.NoError(st.DB().QueryRow(`SELECT messages_updated FROM sync_runs WHERE status = 'paused'`).Scan(&pausedUpdates))
				assert.Equal(int64(1), pausedUpdates)
				assert.Contains(cachedSnippet(), "Original recent")
				sum, err = imp.Import(t.Context(), omi.ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"})
				require.NoError(err)
				assert.Zero(sum.MeetingsAdded)
				assert.Zero(sum.MeetingsUpdated)
				freshness := cacheNeedsBuild(cfg.DatabaseDSN(), analyticsDir)
				require.True(freshness.NeedsBuild)
				require.True(freshness.FullRebuild)
				assert.True(freshness.HasUpdated)
				assert.False(freshness.HasNew)
				assert.False(freshness.HasDerivedDataDrift)
				result, err := buildCache(cfg.DatabaseDSN(), analyticsDir, false)
				require.NoError(err)
				assert.Equal(int64(2), result.ExportedCount)
				assert.Contains(cachedSnippet(), "Revised recent")
				after, err := query.ReadCacheSyncState(analyticsDir)
				require.NoError(err)
				var completedID int64
				require.NoError(st.DB().QueryRow(`SELECT MAX(id) FROM sync_runs WHERE status = 'completed'`).Scan(&completedID))
				assert.Equal(completedID, after.LastCompletedSyncRunID)
				assert.Greater(after.LastCompletedSyncRunID, before.LastCompletedSyncRunID)
				assert.Equal(int64(1), after.LastCacheUpdateCount)
				assert.Equal(before.LastFailedSyncRunCount, after.LastFailedSyncRunCount)
				assert.Equal(before.LastFailedSyncRunIDSum, after.LastFailedSyncRunIDSum)
				assert.Equal(before.DerivedDataRevision, after.DerivedDataRevision)
				assert.False(cacheNeedsBuild(cfg.DatabaseDSN(), analyticsDir).NeedsBuild)
				return
			case "scheduled":
				require.NoError(runConfiguredOmiSync(t.Context(), st, t.TempDir(), source))
			default:
				cfg.Omi = []config.OmiSource{source}
				previousLimit, previousAfter, previousFull := syncOmiLimit, syncOmiAfter, syncOmiFull
				syncOmiLimit, syncOmiAfter, syncOmiFull = 0, "", false
				t.Cleanup(func() { syncOmiLimit, syncOmiAfter, syncOmiFull = previousLimit, previousAfter, previousFull })
				command := &cobra.Command{Use: "sync-omi"}
				command.SetContext(withStoreResolverConfig(t, cfg))
				var output bytes.Buffer
				command.SetOut(&output)
				command.Flags().IntVar(&syncOmiLimit, "limit", 0, "")
				command.Flags().BoolVar(&syncOmiFull, "full", false, "")
				switch mode {
				case "limit zero":
					require.NoError(command.Flags().Parse([]string{"--limit=0"}))
				case "full false":
					require.NoError(command.Flags().Parse([]string{"--full=false"}))
				}
				previousRefresh := rebuildOmiCacheAfterWrite
				rebuildOmiCacheAfterWrite = func(string, *invocation) error { return nil }
				t.Cleanup(func() { rebuildOmiCacheAfterWrite = previousRefresh })
				require.NoError(syncOmiCmd.RunE(command, []string{"work"}))
				if mode != "manual" {
					assert.Contains(output.String(), "replace the unfinished Omi scan")
					raw, err := st.GetMessageRaw(ids["older"])
					require.NoError(err)
					assert.Contains(string(raw), "Original older")
					return
				}
			}
			raw, err := st.GetMessageRaw(ids["older"])
			require.NoError(err)
			assert.Contains(string(raw), "Revised older")
			body, err := st.GetMessageBodyText(ids["older"])
			require.NoError(err)
			assert.Contains(body, "Revised speech")
			latest, err := st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)
			var checkpoint struct {
				Full    bool      `json:"full"`
				Ceiling time.Time `json:"ceiling"`
			}
			require.NoError(json.Unmarshal([]byte(latest.CursorBefore.String), &checkpoint))
			assert.True(checkpoint.Full)
			assert.Equal(fullCeiling, checkpoint.Ceiling.Format(time.RFC3339Nano))
		})
	}
}

func TestConfiguredOmiSyncAndCacheRefresh(t *testing.T) {
	unpacedOmiClients(t)
	for _, failing := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "partial error"}[failing], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource(omi.SourceType, "work")
			require.NoError(err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				offset := r.URL.Query().Get("offset")
				if offset == "400" {
					if failing {
						w.WriteHeader(http.StatusBadRequest)
					} else {
						_, _ = w.Write([]byte("[]"))
					}
					return
				}
				_, _ = fmt.Fprintf(w, `[{"id":"meeting-%s","created_at":"2026-01-01T12:00:00Z","structured":{"title":"Synthetic meeting","overview":"Synthetic summary","action_items":[]},"transcript_segments":[]}]`, offset) // #nosec G705 -- Synthetic response uses application/json; the analyzer does not model response headers.
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

			err = runConfiguredOmiSync(context.Background(), st, t.TempDir(), config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL})
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
	err := runConfiguredOmiSync(context.Background(), st, t.TempDir(), config.OmiSource{Identifier: "missing", AccountEmail: "owner@example.com", APIKey: "test"})
	require.ErrorContains(err, "add-omi missing")
	sources, err := st.ListSources(omi.SourceType)
	require.NoError(err)
	assert.Empty(sources)
}

func TestManualOmiUsesInvocationConfiguration(t *testing.T) {
	unpacedOmiClients(t)
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
		sources, err := st.ListSources(omi.SourceType)
		require.NoError(err)
		require.Len(sources, 1)
		assert.Equal(identifier, sources[0].Identifier)
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, sources[0].ID).Scan(&count))
		assert.Equal(1, count)
		require.NoError(st.Close())
	}
}

func TestDaemonManualOmiQueuesCacheRefreshAfterRealImport(t *testing.T) {
	unpacedOmiClients(t)
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
			_, err = st.GetOrCreateSource(omi.SourceType, "work")
			require.NoError(err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("offset") != "0" {
					_, _ = w.Write([]byte("[]"))
					return
				}
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

// unpacedOmiClients lifts Omi's hourly pacing for fake servers.
func unpacedOmiClients(t *testing.T) {
	t.Helper()
	previous := omi.RequestInterval
	omi.RequestInterval = 0
	t.Cleanup(func() { omi.RequestInterval = previous })
}

// Scheduled runs, aliases, and manual subprocess commands sharing one key must
// share its pacing, or a frequent schedule spends a fresh token on every run.
func TestOmiPacingSpansScheduledRunsAliasesAndManualCommands(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const interval = 300 * time.Millisecond
	previous := omi.RequestInterval
	omi.RequestInterval = interval
	t.Cleanup(func() { omi.RequestInterval = previous })
	originalRefresh := rebuildOmiCacheAfterScheduledSync
	rebuildOmiCacheAfterScheduledSync = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildOmiCacheAfterScheduledSync = originalRefresh })

	var mu sync.Mutex
	var requests []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests = append(requests, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(server.Close)

	st := testutil.NewTestStore(t)
	cfg := lifecycleTestConfig(t.TempDir())
	state := testInvocationWithConfig(cfg)
	sched := scheduler.New(nil).WithLogger(testDiscardLogger())
	t.Cleanup(func() { <-sched.Stop().Done() })
	for _, id := range []string{"work", "alias"} {
		_, err := st.GetOrCreateSource(omi.SourceType, id)
		require.NoError(err)
		// A trailing slash still names the same backend and key.
		baseURL := server.URL
		if id == "alias" {
			baseURL, err = omi.NormalizeBaseURL(baseURL + "/")
			require.NoError(err)
		}
		require.NoError(registerScheduledOmiJob(sched, state, st, config.OmiSource{Identifier: id, AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: baseURL, Enabled: true, Schedule: "* * * * *"}))
	}
	for _, job := range []string{"omi:work", "omi:work", "omi:alias"} {
		require.NoError(sched.TriggerJob(job))
	}
	for _, status := range sched.JobStatus() {
		assert.Empty(status.LastError, status.Name)
	}
	// A manual command runs in a daemon subprocess with its own client.
	_, err := newOmiClient(server.URL, "omi_dev_synthetic", omiPaceDir(cfg)).ListConversations(t.Context(), omi.ListParams{Limit: 1})
	require.NoError(err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(requests, 7)
}

func TestScheduledOmiYieldsWhileWaitingWithCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(omi.SourceType, "work")
	require.NoError(err)
	cfg := lifecycleTestConfig(t.TempDir())
	state := testInvocationWithConfig(cfg)
	oldInterval := omi.RequestInterval
	omi.RequestInterval = time.Hour
	t.Cleanup(func() { omi.RequestInterval = oldInterval })
	oldRefresh := rebuildOmiCacheAfterScheduledSync
	rebuildOmiCacheAfterScheduledSync = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildOmiCacheAfterScheduledSync = oldRefresh })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"synthetic-meeting","created_at":"2026-01-01T12:00:00Z","structured":{"overview":"Summary","action_items":[]},"transcript_segments":[]}]`))
	}))
	t.Cleanup(server.Close)
	gate := api.NewSerialOperationGate()
	sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "omi")).WithLogger(testDiscardLogger())
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(registerScheduledOmiJob(sched, state, st, config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL, Schedule: "0 0 1 1 *"}))
	_, err = sched.StartJob("omi:work")
	require.NoError(err)
	require.Eventually(func() bool {
		checkpoint, err := st.GetLatestCheckpointedSyncByType(src.ID, omi.SourceType)
		return err == nil && checkpoint.MessagesAdded == 0
	}, 10*time.Second, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	release, ok := gate.BeginRequestWorkContext(ctx, "synthetic request")
	require.True(ok, "an archive request acquires coordination while Omi is waiting")
	defer release()
	checkpoint, err := st.GetLatestCheckpointedSyncByType(src.ID, omi.SourceType)
	require.NoError(err)
	var saved struct {
		Ahead []string `json:"ahead"`
	}
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &saved))
	assert.Equal([]string{"synthetic-meeting"}, saved.Ahead)
	assert.Equal(store.SyncStatusPaused, checkpoint.Status)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), src.ID).Scan(&count))
	assert.Equal(0, count)
}

func TestManualOmiContinuesAfterSourceCooldown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	unpacedOmiClients(t)
	markDaemonCLISubprocessForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer omi_dev_deferred" {
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"synthetic-meeting","created_at":"2026-01-01T12:00:00Z","structured":{"overview":"Summary","action_items":[]},"transcript_segments":[]}]`))
	}))
	t.Cleanup(server.Close)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Omi = []config.OmiSource{
		{Identifier: "deferred", AccountEmail: "owner@example.com", APIKey: "omi_dev_deferred", BaseURL: server.URL},
		{Identifier: "ready", AccountEmail: "owner@example.com", APIKey: "omi_dev_ready", BaseURL: server.URL},
	}
	ctx := withStoreResolverConfig(t, cfg)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	for _, source := range cfg.Omi {
		_, err = st.GetOrCreateSource(omi.SourceType, source.Identifier)
		require.NoError(err)
	}
	require.NoError(st.Close())
	oldLimit, oldAfter, oldFull := syncOmiLimit, syncOmiAfter, syncOmiFull
	syncOmiLimit, syncOmiAfter, syncOmiFull = 1, "", false
	t.Cleanup(func() { syncOmiLimit, syncOmiAfter, syncOmiFull = oldLimit, oldAfter, oldFull })
	oldRefresh := rebuildOmiCacheAfterWrite
	refreshes := 0
	rebuildOmiCacheAfterWrite = func(string, *invocation) error { refreshes++; return nil }
	t.Cleanup(func() { rebuildOmiCacheAfterWrite = oldRefresh })
	output := &bytes.Buffer{}
	command := &cobra.Command{Use: syncOmiCmd.Use}
	command.SetContext(ctx)
	command.SetOut(output)
	command.SetErr(&bytes.Buffer{})
	require.NoError(syncOmiCmd.RunE(command, nil))
	assert.Contains(output.String(), "Omi sync for deferred paused")
	assert.Contains(output.String(), "Syncing Omi for ready")
	assert.Equal(1, refreshes)
	st, err = store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	source, err := st.GetSourceByTypeAndIdentifier(omi.SourceType, "ready")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"synthetic-meeting"})
	require.NoError(err)
	assert.Len(ids, 1)
}

func TestAddOmiProbeHonorsProviderCooldown(t *testing.T) {
	unpacedOmiClients(t)
	markDaemonCLISubprocessForTest(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Omi = []config.OmiSource{{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL}}
	command := &cobra.Command{Use: addOmiCmd.Use}
	command.SetContext(withStoreResolverConfig(t, cfg))
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, addOmiCmd.RunE(command, nil), "provider cooldown until")
	assert.Equal(t, 1, requests)
}

func TestManualOmiKeepsFinalizationErrors(t *testing.T) {
	unpacedOmiClients(t)
	markDaemonCLISubprocessForTest(t)
	for _, cause := range []string{"deadline", "cooldown", "interrupt", "HTTP timeout"} {
		t.Run(cause, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if cause == "cooldown" {
					w.Header().Set("Retry-After", "7200")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if cause == "deadline" || cause == "HTTP timeout" {
					cancel(context.DeadlineExceeded)
				} else {
					cancel(context.Canceled)
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			baseURL := server.URL
			if cause == "HTTP timeout" {
				baseURL = "https://omi.example.com"
				previous := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = previous })
				http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
					if err := req.Context().Err(); !assert.NoError(err) {
						return nil, err
					}
					return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: context.DeadlineExceeded}
				})
			}
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Omi = []config.OmiSource{{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: baseURL}}
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			require.NoError(st.InitSchema())
			_, err = st.GetOrCreateSource(omi.SourceType, "work")
			require.NoError(err)
			if cause != "HTTP timeout" {
				_, err = st.DB().Exec(`CREATE TRIGGER synthetic_finalize BEFORE UPDATE OF status ON sync_runs WHEN NEW.status IN ('failed', 'paused') BEGIN SELECT RAISE(ABORT, 'synthetic finalization failure'); END`)
				require.NoError(err)
			}
			require.NoError(st.Close())
			oldLimit, oldAfter, oldFull := syncOmiLimit, syncOmiAfter, syncOmiFull
			syncOmiLimit, syncOmiAfter, syncOmiFull = 0, "", false
			defer func() { syncOmiLimit, syncOmiAfter, syncOmiFull = oldLimit, oldAfter, oldFull }()
			output := &bytes.Buffer{}
			command := &cobra.Command{Use: syncOmiCmd.Use}
			command.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
			command.SetOut(output)
			command.SetErr(&bytes.Buffer{})
			err = syncOmiCmd.RunE(command, nil)
			if cause != "HTTP timeout" {
				require.ErrorContains(err, "synthetic finalization failure")
			}
			if cause == "deadline" || cause == "HTTP timeout" {
				require.ErrorIs(err, context.DeadlineExceeded)
			}
			if cause == "interrupt" {
				require.ErrorIs(err, context.Canceled)
			}
			if cause == "cooldown" {
				var cooldown *omi.CooldownError
				require.ErrorAs(err, &cooldown)
			}
			assert.NotContains(output.String(), "paused")
			assert.NotContains(output.String(), "saved scan")
			assert.NotContains(output.String(), "resume")
		})
	}
}

func TestScheduledOmiCooldownDefersWithoutFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	unpacedOmiClients(t)
	previous := rebuildOmiCacheAfterScheduledSync
	rebuildOmiCacheAfterScheduledSync = func(context.Context, string) error { return nil }
	t.Cleanup(func() { rebuildOmiCacheAfterScheduledSync = previous })
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(omi.SourceType, "work")
	require.NoError(err)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	cfg := lifecycleTestConfig(t.TempDir())
	state := testInvocationWithConfig(cfg)
	sched := scheduler.New(nil).WithLogger(testDiscardLogger())
	defer func() { <-sched.Stop().Done() }()
	require.NoError(registerScheduledOmiJob(sched, state, st, config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic", BaseURL: server.URL, Enabled: true, Schedule: "0 0 1 1 *"}))
	require.Error(sched.TriggerJob("omi:work"))
	require.NotEmpty(sched.JobStatus()[0].LastError)
	require.NoError(sched.TriggerJob("omi:work"))
	assert.Equal(2, requests)
	latest, err := st.GetLatestCheckpointedSyncByType(src.ID, omi.SourceType)
	require.NoError(err)
	assert.Equal(store.SyncStatusPaused, latest.Status)
	rebuildOmiCacheAfterScheduledSync = func(context.Context, string) error { return errors.New("synthetic cache finalization failure") }
	require.NoError(registerScheduledOmiJob(sched, state, st, config.OmiSource{Identifier: "work", AccountEmail: "owner@example.com", APIKey: "omi_dev_synthetic_cache_failure", BaseURL: server.URL, Enabled: true, Schedule: "0 0 1 1 *"}))
	require.ErrorContains(sched.TriggerJob("omi:work"), "synthetic cache finalization failure")
	assert.Contains(sched.JobStatus()[0].LastError, "synthetic cache finalization failure")
}
