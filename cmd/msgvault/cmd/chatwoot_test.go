package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
)

func TestChatwootInboxesFailIndependently(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	var failing atomic.Int64
	var enumerations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/accounts/9/inboxes":
			_, _ = fmt.Fprint(w, `{"payload":[{"id":7},{"id":8}]}`)
		case "/api/v1/accounts/9/agents":
			_, _ = fmt.Fprint(w, `[]`)
		case "/api/v1/accounts/9/conversations":
			enumerations.Add(1)
			if r.URL.Query().Get("inbox_id") == strconv.FormatInt(failing.Load(), 10) {
				http.Error(w, "inbox unavailable", http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, `{"data":{"payload":[]}}`)
		default:
			assert.Fail("unexpected provider request", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	src := config.ChatwootSource{URL: server.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN"}
	_, err := registerChatwootProfile(t.Context(), st, src)
	require.NoError(err)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	state := testInvocationWithConfig(cfg)
	events := 0
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal("sync_failed", event)
		assert.Equal("chatwoot", props["sync_source"])
		events++
		return nil
	}}
	ctx := withInvocation(t.Context(), state)
	for i, id := range []int64{7, 7, 8, 8, 7} {
		failing.Store(id)
		sum, err := importChatwootProfile(ctx, st, src, chatwootRunOptions{}, cfg)
		require.ErrorContains(err, fmt.Sprintf("inbox %d", id))
		assert.NotContains(err.Error(), fmt.Sprintf("inbox %d", 15-id))
		require.NotNil(sum)
		assert.Equal(2, sum.Sources)
		if i == 0 {
			assert.Equal(int32(3), enumerations.Load(), "healthy inbox is listed by activity and enumerated after the failure")
		}
		assert.Equal([]int{1, 1, 2, 2, 3}[i], events)
	}
}

func TestChatwootPreLoopFailureRecordsOnlySelectedInboxes(t *testing.T) {
	for _, mode := range []string{"authentication rejected", "all excluded", "missing token"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/accounts/9/inboxes", r.URL.Path)
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
			}))
			t.Cleanup(server.Close)
			src := config.ChatwootSource{URL: server.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN",
				Inboxes: []int64{7, 8}, ExcludeInboxes: []int64{8}}
			want, message := 1, "discover Chatwoot inboxes"
			switch mode {
			case "missing token":
				require.NoError(os.Unsetenv("EXAMPLE_CHATWOOT_TOKEN"))
				message = "needs EXAMPLE_CHATWOOT_TOKEN in the daemon environment"
			case "all excluded":
				src.ExcludeInboxes = []int64{7, 8}
				want, message = 0, "no included Chatwoot inboxes"
			}
			for _, id := range []string{
				chatwoot.SourceIdentifier(src.URL, 9, 7),
				chatwoot.SourceIdentifier(src.URL, 9, 8),
				chatwoot.SourceIdentifier(src.URL, 9, 9),
				chatwoot.SourceIdentifier(src.URL, 10, 7),
			} {
				_, err := st.GetOrCreateSource(chatwoot.SourceType, id)
				require.NoError(err)
			}
			state := testInvocationWithConfig(lifecycleTestConfig(t.TempDir()))
			events := 0
			state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
				assert.Equal("sync_failed", event)
				assert.Equal("chatwoot", props["sync_source"])
				events++
				return nil
			}}
			ctx := withInvocation(t.Context(), state)

			err := runConfiguredChatwootSync(ctx, st, src)
			require.ErrorContains(err, message)
			assert.Equal(want, events, "only selected inboxes enter failure")
		})
	}
}

func TestChatwootRegisteredInboxSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src := config.ChatwootSource{Identifier: "support", URL: "https://chatwoot.example.com", AccountID: 9, ExcludeInboxes: []int64{8}}
	cfg := &config.Config{Chatwoot: []config.ChatwootSource{src, {Identifier: "sales", URL: src.URL, AccountID: 10}}}
	_, err := resolveChatwootProfiles(nil, cfg, true)
	require.ErrorContains(err, "pass an identifier with --inbox")
	profiles, err := resolveChatwootProfiles(nil, cfg, false)
	require.NoError(err)
	assert.Equal(cfg.Chatwoot, profiles)
	profiles, err = resolveChatwootProfiles([]string{"support"}, cfg, true)
	require.NoError(err)
	assert.Equal([]config.ChatwootSource{src}, profiles)
	cfg.Chatwoot = cfg.Chatwoot[:1]
	profiles, err = resolveChatwootProfiles(nil, cfg, true)
	require.NoError(err)
	assert.Equal(cfg.Chatwoot, profiles)
	for _, id := range []string{chatwoot.SourceIdentifier(src.URL, 9, 7), chatwoot.SourceIdentifier(src.URL, 9, 8), chatwoot.SourceIdentifier(src.URL, 10, 7)} {
		_, err := st.GetOrCreateSource(chatwoot.SourceType, id)
		require.NoError(err)
	}
	ids, err := resolveChatwootSyncInboxes(st, src, nil)
	require.NoError(err)
	assert.Equal([]int64{7}, ids)
	ids, err = resolveChatwootSyncInboxes(st, src, []int64{7, 7})
	require.NoError(err)
	assert.Equal([]int64{7}, ids)
	_, err = resolveChatwootSyncInboxes(st, src, []int64{8})
	require.ErrorContains(err, "excluded")
	_, err = resolveChatwootSyncInboxes(st, src, []int64{99})
	require.ErrorContains(err, "not registered")
	src.Inboxes = []int64{7, 999}
	ids, err = resolveChatwootSyncInboxes(st, src, nil)
	require.NoError(err)
	assert.Equal([]int64{7}, ids)
	src.ExcludeInboxes = append(src.ExcludeInboxes, 999)
	ids, err = resolveChatwootSyncInboxes(st, src, nil)
	require.NoError(err)
	assert.Equal([]int64{7}, ids)
}

func TestChatwootInboxIDsAreDecimal(t *testing.T) {
	require := require.New(t)
	cmd := newSyncChatwootCmd()
	require.NoError(cmd.ParseFlags([]string{"--inbox", "010,08", "--inbox", "9"}))
	values, err := cmd.Flags().GetStringSlice("inbox")
	require.NoError(err)
	ids, err := parseChatwootInboxIDs(values)
	require.NoError(err)
	assert.Equal(t, []int64{10, 8, 9}, ids)
	for _, value := range []string{"0x10", "0", "-1", "9223372036854775808", "invalid"} {
		_, err := parseChatwootInboxIDs([]string{value})
		require.Error(err, value)
	}
}

func TestChatwootSyncRejectsExplicitEmptyInbox(t *testing.T) {
	for _, args := range [][]string{{"--inbox="}, {"--inbox", ""}} {
		cmd := newSyncChatwootCmd()
		require.NoError(t, cmd.ParseFlags(args))
		require.ErrorContains(t, cmd.RunE(cmd, nil), "--inbox IDs must be positive decimal integers")
	}
}

func TestChatwootOptionsPreserveSourcePolicies(t *testing.T) {
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	no := false
	yes := true
	src := config.ChatwootSource{SelfAgentIDs: []int64{201}, IncludePrivate: &yes, Media: &no, MaxMediaMB: 3, ReconcileIntervalHours: 2}
	opts := chatwootImportOptions(src, 7, cfg)
	assert.Equal(int64(7), opts.InboxID)
	assert.Equal([]int64{201}, opts.SelfAgentIDs)
	assert.True(opts.IncludePrivate)
	assert.Equal(attachmentpolicy.SkipAccountPolicy, opts.Policy.DisabledReason)
	assert.Equal(int64(3<<20), opts.Policy.MaxBytes)
	assert.Equal(2*time.Hour, opts.ReconcileInterval)
	assert.Equal(cfg.AttachmentsDir(), opts.AttachmentsDir)
}

func TestChatwootRegistrationUsesDaemonEnvironmentAndExcludesInboxes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("synthetic-token", r.Header.Get("Api_access_token"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/accounts/9/inboxes":
			_, _ = w.Write([]byte(`{"payload":[{"id":7,"name":"Support","channel_type":"Channel::Api","auth_token":"do-not-store"},{"id":8,"name":"Excluded","channel_type":"Channel::Api"}]}`))
		default:
			assert.Fail("unexpected request", r.URL.Path)
		}
	}))
	defer srv.Close()
	registered, err := registerChatwootProfile(context.Background(), st, config.ChatwootSource{Identifier: "support", URL: srv.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN", ExcludeInboxes: []int64{8}})
	require.NoError(err)
	require.Len(registered, 1)
	assert.Equal(srv.URL+"/accounts/9/inboxes/7", registered[0].Identifier)
	sources, err := st.ListSources(chatwoot.SourceType)
	require.NoError(err)
	require.Len(sources, 1)
	assert.NotContains(fmt.Sprintf("%+v", *sources[0]), "do-not-store", "inbox channel credentials are not stored")
}

func TestChatwootSyncWarnsForMissingRegisteredInbox(t *testing.T) {
	for _, mode := range []string{"include", "exclude", "all"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
			var logs bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(logger) })
			var synced atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/accounts/9/inboxes":
					_, _ = w.Write([]byte(`{"payload":[{"id":7}]}`))
				case "/api/v1/accounts/9/agents":
					_, _ = w.Write([]byte(`[]`))
				case "/api/v1/accounts/9/conversations":
					if r.URL.Query().Get("inbox_id") == "999" {
						http.NotFound(w, r)
						return
					}
					synced.Store(true)
					_, _ = w.Write([]byte(`{"data":{"payload":[]}}`))
				default:
					assert.Fail("unexpected request", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			src := config.ChatwootSource{Identifier: "support", URL: server.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN"}
			switch mode {
			case "include":
				src.Inboxes = []int64{7, 999}
			case "exclude":
				src.ExcludeInboxes = []int64{8}
			}
			for _, id := range []int64{7, 999} {
				_, err := st.GetOrCreateSource(chatwoot.SourceType, chatwoot.SourceIdentifier(src.URL, 9, id))
				require.NoError(err)
			}
			sum, err := importChatwootProfile(t.Context(), st, src, chatwootRunOptions{}, config.NewDefaultConfig())
			require.NoError(err)
			assert.Equal(1, sum.Sources)
			assert.True(synced.Load())
			assert.Contains(logs.String(), "inaccessible")
			assert.Contains(logs.String(), "inbox=999")
		})
	}
}

func TestChatwootScheduledJobUsesAccountIdentityAndDefersMediaPacking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newAttachmentMaintenanceFixture(t)
	var hash string
	sched := scheduler.New(func(context.Context, string) error { return nil }).WithLogger(fixture.maintenance.logger)
	t.Cleanup(func() { <-sched.Stop().Done() })
	src := config.ChatwootSource{Identifier: "renamed-profile", URL: "https://chatwoot.example.com/support", AccountID: 9, Enabled: true, Schedule: "*/30 * * * *"}
	calls := 0
	require.NoError(registerScheduledChatwootJob(sched, src, fixture.maintenance, func(context.Context) error {
		calls++
		hash = fixture.ingestLoose([]byte("synthetic Chatwoot scheduled media"))
		return nil
	}))
	job, ok := api.SchedulerJobNameForSource(chatwoot.SourceType, "https://chatwoot.example.com/support/accounts/9/inboxes/7")
	require.True(ok)
	const expectedJob = "chatwoot:https://chatwoot.example.com/support/accounts/9"
	assert.Equal(expectedJob, job)
	require.True(sched.IsJobScheduled(expectedJob))
	require.NoError(sched.TriggerJob(expectedJob))
	assert.Equal(1, calls)
	assert.Nil(fixture.packedEntry(hash), "scheduled sync defers packing")
	assert.True(fixture.maintenance.packPending.Load(), "new media requests a later pack pass")
	require.NoError(fixture.maintenance.runPendingPack(context.Background()))
	assert.NotNil(fixture.packedEntry(hash))
}

func TestChatwootProfileSyncRejectsDisablingArchivedPrivateNotes(t *testing.T) {
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	message := chatwootapi.Message(101, 1700000000, nil)
	message["private"] = true
	fixture := chatwootapi.New(20, []map[string]any{message}, time.Now)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/accounts/3/inboxes" {
			_, _ = w.Write([]byte(`{"payload":[{"id":7}]}`))
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	yes, no := true, false
	src := config.ChatwootSource{Identifier: "support", URL: server.URL, AccountID: 3, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN", IncludePrivate: &yes}
	_, err := st.GetOrCreateSource(chatwoot.SourceType, chatwoot.SourceIdentifier(src.URL, 3, 7))
	require.NoError(err)
	sum, err := importChatwootProfile(t.Context(), st, src, chatwootRunOptions{}, cfg)
	require.NoError(err)
	require.Equal(1, sum.MessagesAdded)
	for _, includePrivate := range []*bool{&no, nil} {
		src.IncludePrivate = includePrivate
		_, err = importChatwootProfile(t.Context(), st, src, chatwootRunOptions{}, cfg)
		require.EqualError(err, "chatwoot inbox 7: chatwoot include_private cannot be turned off after private notes are archived; set include_private = true")
	}
}

func TestChatwootLimitedProfileSyncReportsResumableWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	fixture := chatwootapi.New(20, []map[string]any{
		chatwootapi.Message(101, 1700000000, nil),
		chatwootapi.Message(102, 1700000001, nil),
	}, time.Now)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/accounts/3/inboxes" {
			_, _ = w.Write([]byte(`{"payload":[{"id":7}]}`))
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	src := config.ChatwootSource{Identifier: "support", URL: server.URL, AccountID: 3, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN"}
	_, err := st.GetOrCreateSource(chatwoot.SourceType, chatwoot.SourceIdentifier(src.URL, 3, 7))
	require.NoError(err)
	sum, err := importChatwootProfile(context.Background(), st, src, chatwootRunOptions{Limit: 1}, cfg)
	require.NoError(err)
	require.NotNil(sum)
	assert.True(sum.Partial)
	assert.Equal(1, sum.MessagesProcessed)
	var output bytes.Buffer
	printChatwootSummary(&output, src.Identifier, sum)
	assert.Contains(output.String(), "1 messages processed (1 added)")
	assert.Contains(output.String(), "Unfinished history or artifact work will resume on the next sync.")
}
