package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func facebookProfile() config.FacebookCommentsSource {
	return config.FacebookCommentsSource{Identifier: "example-page", AppID: "9", PageID: "100", TokenEnv: "EXAMPLE_FACEBOOK_PAGE_TOKEN", Enabled: true, Schedule: "*/30 * * * *"}
}

func facebookHTTPFixture(t *testing.T, wrongIdentity bool) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-page-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v26.0/me":
			id := "100"
			if wrongIdentity {
				id = "101"
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":"Example Page","category":"Community"}`, id)
		case "/v26.0/100/posts":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"100_200","message":"Example post","from":{"id":"100","name":"Example Page"}}]}`)
		case "/v26.0/100_200/comments":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"100_201","message":"Example comment","from":{"id":"300","name":"Example Customer"}}]}`)
		case "/v26.0/100_201/comments":
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		case "/v26.0/100/visitor_posts":
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		case "/v26.0/100/ads_posts":
			http.Error(w, "synthetic denial", http.StatusForbidden)
		default:
			assert.Fail(t, "unexpected Graph request", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	require.NoError(t, err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	// Intercept the external boundary only; client, importer and Store remain real.
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Scheme = destination.Scheme
		uri.Host = destination.Host
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
}

func TestFacebookRegistrationRejectsIdentityBeforeWriting(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, true)
	err := registerFacebookCommentsProfile(t.Context(), st, facebookProfile())
	require.Error(t, err)
	sources, err := st.ListSources(meta.SourceType)
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestFacebookRegistrationOutputOmitsSourceID(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
	cmd := newAddFacebookCommentsCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(t, cmd.RunE(cmd, []string{"example-page"}))
	assert.Equal(t, "Registered Facebook Page 100 for example-page\n", output.String())
}

func TestFacebookConfiguredCredentialsSelectionDelegationFlagsAndHelp(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "")
	_, err := configuredFacebookCommentsClient(facebookProfile())
	require.ErrorContains(err, "daemon environment")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
	_, err = resolveFacebookCommentsProfiles([]string{"missing"}, cfg)
	require.Error(err)
	for _, action := range []string{"add", "sync"} {
		leaf, _, err := rootCmd.Find([]string{"source", "facebook-comments", action})
		require.NoError(err)
		assert.False(agentDelegatedCapable(leaf))
	}
	cmd := newSyncFacebookCommentsCmd()
	cmd.SetArgs([]string{"--max-requests", "0"})
	require.ErrorContains(cmd.Execute(), "positive")
	var output bytes.Buffer
	cmd = newSyncFacebookCommentsCmd()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--help"})
	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "--max-requests")
	assert.Contains(output.String(), "Facebook")
	output.Reset()
	cmd = newAddFacebookCommentsCmd()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--help"})
	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "With no identifier, registers all configured Facebook Pages.")
	assert.Contains(output.String(), "Tokens come from the named daemon environment variable.")
}

func TestFacebookManualPartialWritesContinueAcrossProfiles(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	first, second := facebookProfile(), facebookProfile()
	first.Identifier, second.Identifier, second.AppID = "first", "second", "99"
	cfg.FacebookComments = []config.FacebookCommentsSource{first, second}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	for _, profile := range cfg.FacebookComments {
		err := registerFacebookCommentsProfile(t.Context(), st, profile)
		require.NoError(err)
	}
	cmd := newSyncFacebookCommentsCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err := cmd.RunE(cmd, nil)
	require.Error(err)
	assert.Contains(err.Error(), "facebook profile first")
	assert.Contains(err.Error(), "facebook profile second")
	assert.Contains(output.String(), "Partial coverage")
	for _, profile := range cfg.FacebookComments {
		source, err := st.GetSourceByTypeAndIdentifier(meta.SourceType, meta.SourceIdentifier(profile.AppID, profile.PageID))
		require.NoError(err)
		ids, err := st.MessageExistsBatch(source.ID, []string{"100_201"})
		require.NoError(err)
		assert.NotZero(ids["100_201"], "accepted partial writes must remain for %s", profile.Identifier)
		run, err := st.GetLatestSync(source.ID)
		require.NoError(err)
		assert.Equal(store.SyncStatusFailed, run.Status)
		assert.Equal(int64(2), run.MessagesAdded)
	}
}

func TestFacebookPartialWritesRefreshScheduledCache(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	src := facebookProfile()
	err := registerFacebookCommentsProfile(t.Context(), st, src)
	require.NoError(t, err)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	cfg.FacebookComments = []config.FacebookCommentsSource{src}
	original := rebuildFacebookCommentsCacheAfterScheduledSync
	defer func() { rebuildFacebookCommentsCacheAfterScheduledSync = original }()
	refreshes := 0
	rebuildFacebookCommentsCacheAfterScheduledSync = func(ctx context.Context, identifier string) error {
		refreshes++
		assert.Equal(t, "facebook_comments:app:9:page:100", identifier)
		ids, err := st.MessageExistsBatch(1, []string{"100_201"})
		require.NoError(t, err)
		assert.NotZero(t, ids["100_201"])
		return nil
	}
	err = runConfiguredFacebookCommentsSync(withInvocation(t.Context(), testInvocationWithConfig(cfg)), st, src)
	require.Error(t, err)
	assert.Equal(t, 1, refreshes)
}

func TestFacebookSchedulerUsesExactConfiguredJob(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	fixture := newAttachmentMaintenanceFixture(t)
	sched := scheduler.New(func(context.Context, string) error { return errors.New("account dispatcher must not run") }).WithLogger(fixture.maintenance.logger)
	t.Cleanup(func() { <-sched.Stop().Done() })
	calls := 0
	require.NoError(registerScheduledFacebookCommentsJob(sched, facebookProfile(), fixture.maintenance, func(context.Context) error { calls++; return nil }))
	name, ok := api.SchedulerJobNameForSource("facebook_comments", "app:9:page:100")
	require.True(ok)
	require.True(sched.IsJobScheduled(name))
	require.NoError(sched.TriggerJob(name))
	assert.Equal(1, calls)
	require.Error(sched.TriggerJob("facebook_comments:example-page"))
}

func TestFacebookScheduledTextSyncDoesNotWaitForAttachmentMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		// Real coordinator and job registration: a text-only job must run
		// while blob maintenance owns its exclusive lease. No archive or blob
		// directory is needed to exercise this scheduler boundary.
		maintenance := &attachmentMaintenance{coordinator: packstore.NewCoordinator()}
		lease, err := maintenance.coordinator.AcquireMaintenance(t.Context())
		require.NoError(err)
		sched := scheduler.New(func(context.Context, string) error {
			return errors.New("unexpected account dispatch")
		})
		defer func() { <-sched.Stop().Done() }()
		entered := make(chan struct{})
		done := make(chan error, 1)
		require.NoError(registerScheduledFacebookCommentsJob(sched, facebookProfile(), maintenance, func(context.Context) error {
			close(entered)
			return nil
		}))
		go func() { done <- sched.TriggerJob("facebook_comments:app:9:page:100") }()
		synctest.Wait()
		ranBeforeRelease := false
		select {
		case <-entered:
			ranBeforeRelease = true
		default:
		}
		// Release and join even in RED, so the regression cannot strand a job.
		require.NoError(lease.Release())
		require.NoError(<-done)
		assert.True(t, ranBeforeRelease, "text-only scheduled sync waited for an unrelated attachment lease")
	})
}

func TestFacebookGroupedCommandContract(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assert, require := assert.New(t), require.New(t)
	for _, action := range []string{"add", "sync"} {
		leaf, args, err := rootCmd.Find([]string{"source", "facebook-comments", action})
		require.NoError(err)
		assert.Empty(args)
		assert.Equal("msgvault source facebook-comments "+action, leaf.CommandPath())
		assert.False(leaf.Hidden)
		assert.Empty(leaf.Aliases)
		forwarded, err := daemonCLIArgsFromCobra(leaf, []string{"example-page"})
		require.NoError(err)
		assert.Equal([]string{"source", "facebook-comments", action, "example-page"}, forwarded)
		assert.False(agentDelegatedCapable(leaf))
	}
	for _, command := range rootCmd.Commands() {
		assert.NotContains(command.Name(), "facebook-comments", "provider commands belong under source")
	}
	assert.True(manualSyncCLICommand([]string{"source", "facebook-comments", "sync", "example-page"}))
	assert.False(manualSyncCLICommand([]string{"source", "facebook-comments", "add", "example-page"}))
}

func TestFacebookSourceErrorShapes(t *testing.T) {
	t.Run("source not registered", func(t *testing.T) {
		facebookHTTPFixture(t, false)
		t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
		st := testutil.NewSQLiteTestStore(t)
		_, err := importFacebookCommentsProfile(t.Context(), st, facebookProfile(), meta.ImportOptions{})
		require.ErrorIs(t, err, store.ErrSourceNotFound)
		assert.Equal(t, opserr.KindNotFound, opserr.KindOf(err))
	})
	t.Run("missing profile", func(t *testing.T) {
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
		_, err := resolveFacebookCommentsProfiles([]string{"missing"}, cfg)
		require.Error(t, err)
		assert.Equal(t, opserr.KindNotFound, opserr.KindOf(err))
	})
	t.Run("invalid budget", func(t *testing.T) {
		cmd := newSyncFacebookCommentsCmd()
		cmd.SetArgs([]string{"--max-requests", "0"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, opserr.KindInvalid, opserr.KindOf(err))
	})
	for _, test := range []struct {
		name string
		code int
		run  func(t *testing.T) error
	}{
		{"not configured", 5, func(t *testing.T) error {
			t.Helper()
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			_, err := resolveFacebookCommentsProfiles(nil, cfg)
			return err
		}},
		{"missing credential", 7, func(t *testing.T) error {
			t.Helper()
			t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "")
			_, err := configuredFacebookCommentsClient(facebookProfile())
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			err := test.run(t)
			require.Error(err)
			var classified interface {
				error
				ExitCode() int
				Hint() string
			}
			require.ErrorAs(err, &classified)
			assert.Equal(test.code, classified.ExitCode())
			assert.NotEmpty(classified.Hint())
		})
	}
}

func TestFacebookProviderErrorShape(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	client, err := configuredFacebookCommentsClient(facebookProfile())
	require.NoError(err)
	_, err = client.Posts(t.Context(), "100", meta.AdsPosts, "")
	require.Error(err)
	var cause *meta.APIError
	require.ErrorAs(err, &cause)
	assert.Equal(http.StatusForbidden, cause.Status)
	var coded interface {
		error
		APIErrorCode() string
	}
	require.ErrorAs(err, &coded)
	assert.Equal("source_scope_unavailable", coded.APIErrorCode())
}

func TestFacebookSourceStateConflictShapes(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(strconv.FormatBool(retired), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
			facebookHTTPFixture(t, false)
			st := testutil.NewSQLiteTestStore(t)
			src, err := st.GetOrCreateSourceContext(t.Context(), meta.SourceType, meta.SourceIdentifier("9", "100"))
			require.NoError(err)
			want := store.ErrSyncAlreadyActive
			if retired {
				target, err := st.GetOrCreateSourceContext(t.Context(), meta.SourceType, meta.SourceIdentifier("99", "100"))
				require.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: target.ID})
				require.NoError(err)
				want = store.ErrSourceRetired
			} else {
				execution, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
				require.NoError(err)
				t.Cleanup(func() { require.NoError(execution.Release()) })
			}
			_, err = importFacebookCommentsProfile(t.Context(), st, facebookProfile(), meta.ImportOptions{})
			require.ErrorIs(err, want)
			var classified interface {
				error
				ExitCode() int
				Hint() string
			}
			require.ErrorAs(err, &classified)
			assert.Equal(6, classified.ExitCode())
			assert.NotEmpty(classified.Hint())
		})
	}
}
