package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/scheduler"
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
			http.Error(w, "synthetic denial", 403)
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
			return nil, fmt.Errorf("unexpected fixture host")
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
	_, err := registerFacebookCommentsProfile(t.Context(), st, facebookProfile())
	require.Error(t, err)
	sources, err := st.ListSources(meta.SourceType)
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestFacebookConfiguredCredentialAndSelection(t *testing.T) {
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "")
	_, err := configuredFacebookCommentsClient(facebookProfile())
	require.ErrorContains(t, err, "daemon environment")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
	_, err = resolveFacebookCommentsProfiles([]string{"missing"}, cfg)
	require.Error(t, err)
	for _, command := range []string{"add-facebook-comments", "sync-facebook-comments"} {
		assert.False(t, agentDelegatedCommand(command))
	}
	cmd := newSyncFacebookCommentsCmd()
	cmd.SetArgs([]string{"--max-requests", "0"})
	require.ErrorContains(t, cmd.Execute(), "positive")
	var output bytes.Buffer
	cmd = newSyncFacebookCommentsCmd()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "--max-requests")
	assert.Contains(t, output.String(), "Facebook")
}

func TestFacebookPartialWritesRefreshScheduledCache(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	facebookHTTPFixture(t, false)
	src := facebookProfile()
	_, err := registerFacebookCommentsProfile(t.Context(), st, src)
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
	fixture := newAttachmentMaintenanceFixture(t)
	sched := scheduler.New(func(context.Context, string) error { return fmt.Errorf("account dispatcher must not run") }).WithLogger(fixture.maintenance.logger)
	t.Cleanup(func() { <-sched.Stop().Done() })
	calls := 0
	require.NoError(t, registerScheduledFacebookCommentsJob(sched, facebookProfile(), fixture.maintenance, func(context.Context) error { calls++; return nil }))
	name, ok := api.SchedulerJobNameForSource("facebook_comments", "app:9:page:100")
	require.True(t, ok)
	require.True(t, sched.IsJobScheduled(name))
	require.NoError(t, sched.TriggerJob(name))
	assert.Equal(t, 1, calls)
	assert.Error(t, sched.TriggerJob("facebook_comments:example-page"))
}

func TestFacebookScheduledTextSyncDoesNotWaitForAttachmentMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Real coordinator and job registration: a text-only job must run
		// while blob maintenance owns its exclusive lease. No archive or blob
		// directory is needed to exercise this scheduler boundary.
		maintenance := &attachmentMaintenance{coordinator: packstore.NewCoordinator()}
		lease, err := maintenance.coordinator.AcquireMaintenance(t.Context())
		require.NoError(t, err)
		sched := scheduler.New(func(context.Context, string) error {
			return fmt.Errorf("unexpected account dispatch")
		})
		defer func() { <-sched.Stop().Done() }()
		entered := make(chan struct{})
		done := make(chan error, 1)
		require.NoError(t, registerScheduledFacebookCommentsJob(sched, facebookProfile(), maintenance, func(context.Context) error {
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
		require.NoError(t, lease.Release())
		require.NoError(t, <-done)
		assert.True(t, ranBeforeRelease, "text-only scheduled sync waited for an unrelated attachment lease")
	})
}
