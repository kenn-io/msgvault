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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func TestInstagramMentionsCommandsRequireOptIn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	profile := config.InstagramCommentsSource{Identifier: "example", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_MENTION_TOKEN"}
	t.Setenv(profile.TokenEnv, "")
	cfg.InstagramComments = []config.InstagramCommentsSource{profile}
	_, err := resolveInstagramMentionsProfiles([]string{"example"}, cfg)
	requirements.EqualError(err, `instagram profile "example" does not enable mentions`)
	assertions.Equal(opserr.KindInvalid, opserr.KindOf(err))
	_, err = registerInstagramMentionsProfile(t.Context(), nil, profile)
	requirements.EqualError(err, `instagram profile "example" does not enable mentions`)
	profile.Mentions = true
	cfg.InstagramComments = append(cfg.InstagramComments, profile)
	cfg.InstagramComments[1].Identifier = "enabled"
	selected, err := resolveInstagramMentionsProfiles(nil, cfg)
	requirements.NoError(err)
	requirements.Len(selected, 1)
	assertions.Equal("enabled", selected[0].Identifier)
	_, err = resolveInstagramMentionsProfiles([]string{"missing"}, cfg)
	requirements.Error(err)
	assertions.Equal(opserr.KindNotFound, opserr.KindOf(err))
	_, err = registerInstagramMentionsProfile(t.Context(), nil, profile)
	requirements.EqualError(err, `instagram profile "example" needs EXAMPLE_MENTION_TOKEN in the daemon environment`)
	var diagnostic interface {
		error
		ExitCode() int
		Hint() string
	}
	requirements.ErrorAs(err, &diagnostic)
	assertions.Equal(7, diagnostic.ExitCode())
	assertions.Contains(diagnostic.Hint(), "daemon environment")
}

func TestInstagramMentionsConfiguredCommandsAndPartialWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	profile := config.InstagramCommentsSource{Identifier: "example", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_MENTION_TOKEN", Mentions: true, Enabled: true, Schedule: "*/30 * * * *"}
	t.Setenv(profile.TokenEnv, "synthetic-mention-token")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-mention-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_account"}}`)
		case "/v26.0/200/tags":
			if r.URL.Query().Get("after") != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, `{"data":[{"id":"300","caption":"synthetic tag","username":"example_user"}],"paging":{"cursors":{"after":"next"}}}`)
		default:
			assert.Fail(t, "unexpected fixture route", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	requirements.NoError(err)
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
	cfg.InstagramComments = []config.InstagramCommentsSource{profile}
	st, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchema())
	var diagnostic interface {
		error
		ExitCode() int
		Hint() string
	}
	_, err = importInstagramMentionsProfile(t.Context(), st, profile, meta.ImportOptions{})
	requirements.ErrorAs(err, &diagnostic)
	assertions.Equal(5, diagnostic.ExitCode())
	assertions.Contains(diagnostic.Hint(), "source instagram-mentions add")
	var output bytes.Buffer
	add := newAddInstagramMentionsCmd()
	add.SetOut(&output)
	add.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	requirements.NoError(add.RunE(add, []string{"EXAMPLE"}))
	cmd := newSyncInstagramMentionsCmd()
	cmd.SetOut(&output)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err = cmd.RunE(cmd, nil)
	requirements.ErrorContains(err, "HTTP 403")
	requirements.ErrorAs(err, &diagnostic)
	assertions.Equal(7, diagnostic.ExitCode())
	assertions.Contains(diagnostic.Hint(), "permissions")
	assertions.Contains(output.String(), "Partial coverage")
	src, err := st.GetSourceByTypeAndIdentifier(meta.MentionsSourceType, "app:9:page:100:instagram:200")
	requirements.NoError(err)
	ids, err := st.MessageExistsBatch(src.ID, []string{"300"})
	requirements.NoError(err)
	assertions.Len(ids, 1)
	lock, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	requirements.NoError(err)
	_, syncErr := importInstagramMentionsProfile(t.Context(), st, profile, meta.ImportOptions{})
	requirements.NoError(lock.Release())
	requirements.ErrorAs(syncErr, &diagnostic)
	assertions.Equal(6, diagnostic.ExitCode())
	requirements.ErrorIs(syncErr, store.ErrSyncAlreadyActive)
	assertions.True(manualSyncCLICommand([]string{"source", "instagram-mentions", "sync", "example"}))
	assertions.False(agentDelegatedCommand("source"))
	cmd = newSyncInstagramMentionsCmd()
	cmd.SetArgs([]string{"--max-requests", "0"})
	err = cmd.Execute()
	requirements.ErrorContains(err, "positive")
	assertions.Equal(opserr.KindInvalid, opserr.KindOf(err))
	fixture := newAttachmentMaintenanceFixture(t)
	sched := scheduler.New(func(context.Context, string) error { return errors.New("unexpected account dispatch") })
	t.Cleanup(func() { <-sched.Stop().Done() })
	calls := 0
	requirements.NoError(registerScheduledInstagramMentionsJob(sched, profile, fixture.maintenance, func(context.Context) error { calls++; return nil }))
	job, ok := api.SchedulerJobNameForSource(meta.MentionsSourceType, src.Identifier)
	requirements.True(ok)
	requirements.NoError(sched.TriggerJob(job))
	assertions.Equal(1, calls)
}

func TestInstagramMentionsGroupedCLI(t *testing.T) {
	for _, action := range []string{"add", "sync"} {
		t.Run(action, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			args := []string{"source", "instagram-mentions", action, "example"}
			command, remaining, err := rootCmd.Find(args)
			requirements.NoError(err)
			assertions.Equal(action, command.Name())
			assertions.Equal([]string{"example"}, remaining)
			assertions.Empty(command.Aliases)
			forwarded, err := daemonCLIArgsFromCobra(command, remaining)
			requirements.NoError(err)
			assertions.Equal(args, forwarded)
		})
	}
	for _, command := range rootCmd.Commands() {
		assert.NotContains(t, command.Name(), "instagram-mentions")
	}
	assert.False(t, manualSyncCLICommand([]string{"source", "instagram-mentions", "add", "example"}))
}
