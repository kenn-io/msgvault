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
)

func TestInstagramConfiguredCommandsAndPartialWrites(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	profile := config.InstagramCommentsSource{Identifier: "example-account", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_INSTAGRAM_PAGE_TOKEN", Enabled: true, Schedule: "*/30 * * * *"}
	t.Setenv(profile.TokenEnv, "")
	_, err := configuredInstagramCommentsClient(profile)
	require.ErrorContains(err, "daemon environment")
	t.Setenv(profile.TokenEnv, "synthetic-page-token")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := "synthetic-page-token"
		if r.URL.Path == "/v26.0/act_700/adcreatives" {
			token = "synthetic-ad-token"
		}
		assert.Equal("Bearer "+token, r.Header.Get("Authorization"), r.URL.Path)
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200"}}`)
		case "/v26.0/200/media":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"300","caption":"Example media","owner":{"id":"200"}}]}`)
		case "/v26.0/300/comments":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"400","text":"Example comment","parent_id":"300"}]}`)
		case "/v26.0/act_700/adcreatives":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"600","effective_instagram_media_id":"900"}]}`)
		case "/v26.0/900":
			_, _ = fmt.Fprint(w, `{"id":"900","caption":"Ad-only media","owner":{"id":"200"}}`)
		case "/v26.0/900/comments":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"1000","text":"Ad comment"}]}`)
		case "/v26.0/400/replies", "/v26.0/1000/replies":
			http.Error(w, "synthetic denial", http.StatusForbidden)
		default:
			assert.Fail("unexpected fixture request", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	require.NoError(err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Scheme, uri.Host = destination.Scheme, destination.Host
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	second := profile
	second.Identifier, second.AppID = "second", "99"
	ads := profile
	ads.Identifier, ads.AppID, ads.AdAccountID, ads.AdTokenEnv = "ads", "999", "700", "EXAMPLE_META_AD_TOKEN"
	cfg.InstagramComments = []config.InstagramCommentsSource{profile, second, ads}
	_, err = resolveInstagramCommentsProfiles([]string{"missing"}, cfg)
	require.Error(err)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	for _, source := range cfg.InstagramComments {
		_, err := registerInstagramCommentsProfile(t.Context(), st, source)
		require.NoError(err)
	}
	t.Setenv(ads.AdTokenEnv, "")
	sum, err := importInstagramCommentsProfile(t.Context(), st, ads, meta.ImportOptions{})
	require.ErrorContains(err, ads.AdTokenEnv, "the Page token is never used for ad reads")
	assert.Nil(sum)
	t.Setenv(ads.AdTokenEnv, "synthetic-ad-token")
	cmd := newSyncInstagramCommentsCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err = cmd.RunE(cmd, nil)
	require.Error(err)
	assert.Contains(err.Error(), "instagram profile example-account")
	assert.Contains(err.Error(), "instagram profile second")
	assert.Contains(err.Error(), "instagram profile ads")
	assert.Contains(output.String(), "Partial coverage")
	for _, source := range cfg.InstagramComments {
		row, err := st.GetSourceByTypeAndIdentifier(t.Context(), meta.InstagramSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID))
		require.NoError(err)
		ids, err := st.MessageExistsBatch(row.ID, []string{"300", "400", "900", "1000"})
		require.NoError(err)
		if source.AdAccountID != "" {
			assert.Len(ids, 4, "the ads profile also archives ad-only media and its comments")
		} else {
			assert.Len(ids, 2)
		}
		run, err := st.GetLatestSync(row.ID)
		require.NoError(err)
		assert.Equal(store.SyncStatusFailed, run.Status)
	}
	add := newAddInstagramCommentsCmd()
	add.SetOut(&output)
	add.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(add.RunE(add, []string{"EXAMPLE-ACCOUNT"}))
	assert.Contains(output.String(), "Registered Instagram professional account 200")
	for _, action := range []string{"add", "sync"} {
		leaf, _, err := rootCmd.Find([]string{"source", "instagram-comments", action})
		require.NoError(err)
		assert.False(agentDelegatedCapable(leaf))
	}
	cmd = newSyncInstagramCommentsCmd()
	cmd.SetArgs([]string{"--max-requests", "0"})
	require.ErrorContains(cmd.Execute(), "positive")
	fixture := newAttachmentMaintenanceFixture(t)
	sched := scheduler.New(func(context.Context, string) error { return errors.New("unexpected account dispatcher") }).WithLogger(fixture.maintenance.logger)
	t.Cleanup(func() { <-sched.Stop().Done() })
	calls := 0
	require.NoError(registerScheduledInstagramCommentsJob(sched, profile, fixture.maintenance, func(context.Context) error { calls++; return nil }))
	job, ok := api.SchedulerJobNameForSource(meta.InstagramSourceType, meta.InstagramSourceIdentifier(profile.AppID, profile.PageID, profile.InstagramAccountID))
	require.True(ok)
	require.NoError(sched.TriggerJob(job))
	assert.Equal(1, calls)
	require.Error(sched.TriggerJob("instagram_comments:example-account"))
}

func TestInstagramScheduledTextSyncDoesNotWaitForAttachmentMaintenance(t *testing.T) {
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
		require.NoError(registerScheduledInstagramCommentsJob(sched, config.InstagramCommentsSource{AppID: "9", PageID: "100", InstagramAccountID: "200", Schedule: "*/30 * * * *"}, maintenance, func(context.Context) error {
			close(entered)
			return nil
		}))
		go func() { done <- sched.TriggerJob("instagram_comments:app:9:page:100:instagram:200") }()
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

func TestInstagramGroupedCommandContract(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assert, require := assert.New(t), require.New(t)
	for _, action := range []string{"add", "sync"} {
		leaf, args, err := rootCmd.Find([]string{"source", "instagram-comments", action})
		require.NoError(err)
		assert.Empty(args)
		assert.Equal("msgvault source instagram-comments "+action, leaf.CommandPath())
		assert.False(leaf.Hidden)
		assert.Empty(leaf.Aliases)
		forwarded, err := daemonCLIArgsFromCobra(leaf, []string{"example-account"})
		require.NoError(err)
		assert.Equal([]string{"source", "instagram-comments", action, "example-account"}, forwarded)
		assert.False(agentDelegatedCapable(leaf))
	}
	for _, command := range rootCmd.Commands() {
		assert.NotContains(command.Name(), "instagram-comments")
	}
	assert.True(manualSyncCLICommand([]string{"source", "instagram-comments", "sync", "example-account"}))
	assert.False(manualSyncCLICommand([]string{"source", "instagram-comments", "add", "example-account"}))
}

func TestInstagramSourceErrorShapes(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		_, err := resolveInstagramCommentsProfiles(nil, cfg)
		require.Error(err)
		var classified interface {
			error
			ExitCode() int
			Hint() string
		}
		require.ErrorAs(err, &classified)
		assert.Equal(5, classified.ExitCode())
		assert.NotEmpty(classified.Hint())
	})
	t.Run("missing profile", func(t *testing.T) {
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		_, err := resolveInstagramCommentsProfiles([]string{"missing"}, cfg)
		require.Error(t, err)
		assert.Equal(t, opserr.KindNotFound, opserr.KindOf(err))
	})
	t.Run("missing credential", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		profile := config.InstagramCommentsSource{Identifier: "example-account", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_INSTAGRAM_PAGE_TOKEN"}
		t.Setenv(profile.TokenEnv, "")
		_, err := configuredInstagramCommentsClient(profile)
		require.Error(err)
		var classified interface {
			error
			ExitCode() int
			Hint() string
		}
		require.ErrorAs(err, &classified)
		assert.Equal(7, classified.ExitCode())
		assert.NotEmpty(classified.Hint())
	})
	t.Run("invalid budget", func(t *testing.T) {
		cmd := newSyncInstagramCommentsCmd()
		cmd.SetArgs([]string{"--max-requests", "0"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, opserr.KindInvalid, opserr.KindOf(err))
	})
}
