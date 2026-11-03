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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAddFacebookMessagesRoutesThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{"source", "facebook-messages", "add", "example"}, req.Args)
	}, `{"type":"stdout","data":"registered fixture\n"}`, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")
	cmd := messagesTestCommand()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"source", "facebook-messages", "add", "example"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(cmd.Execute())
	assert.Equal(int32(1), requests.Load())
	assert.Equal("registered fixture\n", output.String())
}

func TestMetaMessagesSubscribeInboxOnly(t *testing.T) {
	for _, kind := range []string{meta.SourceTypeFacebookMessages, meta.SourceTypeInstagramMessages} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := webhookTestConfig(t)
			cfg.FacebookComments = nil
			profile := config.MetaMessagesSource{Identifier: "example-inbox", AppID: "9", PageID: "100", TokenEnv: "EXAMPLE_INBOX_TOKEN"}
			if kind == meta.SourceTypeFacebookMessages {
				cfg.FacebookMessages = []config.MetaMessagesSource{profile}
			} else {
				profile.InstagramAccountID = "1784"
				cfg.InstagramMessages = []config.MetaMessagesSource{profile}
			}
			t.Setenv(profile.TokenEnv, "synthetic-page-token")
			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			requests, posted := 0, false
			subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch {
				case r.URL.Path == "/v26.0/app":
					_, _ = fmt.Fprint(w, `{"id":"9"}`)
				case r.Method == http.MethodPost:
					if !assert.NoError(r.ParseForm()) {
						return
					}
					assert.Equal("messages", r.Form.Get("subscribed_fields"))
					posted = true
					_, _ = fmt.Fprint(w, `{"success":true}`)
				default:
					assert.Equal("/v26.0/100/subscribed_apps", r.URL.Path)
					fields := "[]"
					if posted {
						fields = `["messages"]`
					}
					_, _ = fmt.Fprintf(w, `{"data":[{"id":"9","subscribed_fields":%s}]}`, fields)
				}
			})
			run := func(identifier string) error {
				cmd := newSubscribeMetaWebhooksCmd()
				cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				require.NoError(cmd.Flags().Set("fields", "messages"))
				return cmd.RunE(cmd, []string{identifier})
			}
			require.NoError(run(profile.Identifier))
			assert.Equal(4, requests)
			comment := facebookProfile()
			comment.Identifier = profile.Identifier
			cfg.FacebookComments = []config.FacebookCommentsSource{comment}
			requests = 0
			require.Error(run(profile.Identifier))
			assert.Zero(requests, "ambiguous profiles must fail before provider access")
			require.NoError(run(kind + ":" + profile.Identifier))
			assert.Equal(4, requests)
		})
	}
}
func TestSyncInstagramMessagesFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{"source", "instagram-messages", "sync", "--full", "--max-requests=5", "example"}, req.Args)
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")
	cmd := messagesTestCommand()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"source", "instagram-messages", "sync", "example", "--full", "--max-requests", "5"})
	require.NoError(cmd.Execute())
	assert.Equal(int32(1), requests.Load())
	cmd = newSyncMetaMessagesCmd(meta.PlatformInstagram)
	cmd.SetArgs([]string{"--max-requests", "0"})
	require.ErrorContains(cmd.Execute(), "positive")

	help := newSyncMetaMessagesCmd(meta.PlatformInstagram)
	var helpOutput bytes.Buffer
	help.SetOut(&helpOutput)
	help.SetErr(&helpOutput)
	help.SetArgs([]string{"--help"})
	require.NoError(help.Execute())
	assert.Contains(helpOutput.String(), "rescan the inbox; preserve archived history")
}
func TestMetaMessagesMissingTokenEnv(t *testing.T) {
	for _, p := range []meta.DMPlatform{meta.PlatformMessenger, meta.PlatformInstagram} {
		t.Run(string(p), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			t.Setenv("EXAMPLE_INBOX_TOKEN", "")
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Data.DataDir = t.TempDir()
			profile := config.MetaMessagesSource{Identifier: "example", AppID: "9", PageID: "100", TokenEnv: "EXAMPLE_INBOX_TOKEN"}
			label := "facebook"
			if p == meta.PlatformInstagram {
				label = "instagram"
				profile.InstagramAccountID = "1784"
				cfg.InstagramMessages = []config.MetaMessagesSource{profile}
			} else {
				cfg.FacebookMessages = []config.MetaMessagesSource{profile}
			}

			_, clientErr := configuredMetaMessagesClient(p, profile)
			require.EqualError(clientErr, label+` messages profile "example" needs EXAMPLE_INBOX_TOKEN in the daemon environment`)
			var classified interface {
				error
				ExitCode() int
			}
			require.ErrorAs(clientErr, &classified)
			assert.Equal(7, classified.ExitCode())
			t.Setenv("EXAMPLE_INBOX_TOKEN", "synthetic-secret\ninvalid")
			_, clientErr = configuredMetaMessagesClient(p, profile)
			require.Error(clientErr)
			assert.NotContains(clientErr.Error(), "synthetic-secret")
			t.Setenv("EXAMPLE_INBOX_TOKEN", "")
			cmd := newAddMetaMessagesCmd(p)
			cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			cmd.SetArgs([]string{})
			err := cmd.Execute()
			want := label + ` messages profile "example" needs EXAMPLE_INBOX_TOKEN in the daemon environment`
			require.EqualError(err, want)

			syncCmd := newSyncMetaMessagesCmd(p)
			syncCmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			require.EqualError(syncCmd.RunE(syncCmd, nil), want)
		})
	}
}
func TestManualSyncCacheRefreshMessages(t *testing.T) {
	assert := assert.New(t)

	for _, provider := range []string{"facebook-messages", "instagram-messages"} {
		assert.True(manualSyncCLICommand([]string{"source", provider, "sync", "example"}))
		assert.False(manualSyncCLICommand([]string{"source", provider, "add", "example"}))
	}
}
func TestMetaMessagesScheduledJob(t *testing.T) {
	for _, p := range []meta.DMPlatform{meta.PlatformMessenger, meta.PlatformInstagram} {
		t.Run(string(p), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			fixture := newAttachmentMaintenanceFixture(t)
			sched := scheduler.New(func(context.Context, string) error { return errors.New("unexpected account dispatcher") }).WithLogger(fixture.maintenance.logger)
			defer func() { <-sched.Stop().Done() }()
			profile := config.MetaMessagesSource{Identifier: "example", AppID: "9", PageID: "100", Schedule: "*/30 * * * *"}
			want := "facebook_messages:app:9:page:100"
			if p == meta.PlatformInstagram {
				profile.InstagramAccountID = "1784"
				want = "instagram_messages:app:9:page:100:instagram:1784"
			}
			calls := 0
			require.NoError(registerScheduledMetaMessagesJob(sched, p, profile, fixture.maintenance, func(context.Context) error { calls++; return nil }))
			require.True(sched.IsJobScheduled(want))
			require.NoError(sched.TriggerJob(want))
			assert.Equal(1, calls)
		})
	}
}

func TestMetaMessagesCommandArchivesFixture(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_INBOX_TOKEN", "synthetic-inbox-token")
	t.Setenv("EXAMPLE_INBOX_TOKEN_TWO", "synthetic-inbox-token-two")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageID, threadID, messageID, senderID := "100", "t_1", "m_1", "300"
		switch r.Header.Get("Authorization") {
		case "Bearer synthetic-inbox-token":
		case "Bearer synthetic-inbox-token-two":
			pageID, threadID, messageID, senderID = "101", "t_2", "m_2", "301"
		default:
			assert.Fail("unexpected Graph fixture authorization", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":"Example Page","category":"Community"}`, pageID)
		case "/v26.0/" + pageID + "/conversations":
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"updated_time":"2026-11-02T00:00:00Z"}]}`, threadID)
		case "/v26.0/" + threadID:
			_, _ = fmt.Fprintf(w, `{"messages":{"data":[{"id":%q}]}}`, messageID)
		case "/v26.0/" + messageID:
			_, _ = fmt.Fprintf(w, `{"id":%q,"created_time":"2026-11-02T00:00:00Z","from":{"id":%q},"to":{"data":[{"id":%q}]},"message":"command inbox fixture"}`, messageID, senderID, pageID)
		default:
			assert.Fail("unexpected Graph fixture request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	destination, err := url.Parse(server.URL)
	require.NoError(err)
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
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
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	cfg.FacebookMessages = []config.MetaMessagesSource{
		{Identifier: "example", AppID: "9", PageID: "100", TokenEnv: "EXAMPLE_INBOX_TOKEN"},
		{Identifier: "example-two", AppID: "9", PageID: "101", TokenEnv: "EXAMPLE_INBOX_TOKEN_TWO"},
	}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	// An unregistered configured inbox fails before any provider request.
	missing := messagesTestCommand()
	missing.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	missing.SetArgs([]string{"source", "facebook-messages", "sync"})
	var missingOutput bytes.Buffer
	missing.SetOut(&missingOutput)
	missing.SetErr(&missingOutput)
	err = missing.Execute()
	var classified interface {
		error
		ExitCode() int
	}
	require.ErrorAs(err, &classified)
	assert.Equal(4, classified.ExitCode())
	add := messagesTestCommand()
	add.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	add.SetArgs([]string{"source", "facebook-messages", "add"})
	var output bytes.Buffer
	add.SetOut(&output)
	require.NoError(add.Execute())
	sync := messagesTestCommand()
	sync.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	sync.SetArgs([]string{"source", "facebook-messages", "sync"})
	sync.SetOut(&output)
	require.NoError(sync.Execute())
	src, err := st.GetSourceByTypeAndIdentifier(meta.SourceTypeFacebookMessages, "app:9:page:100")
	require.NoError(err)
	rows, err := st.MessageExistsBatch(src.ID, []string{"m_1"})
	require.NoError(err)
	require.NotZero(rows["m_1"])
	text, err := st.GetMessageBodyText(rows["m_1"])
	require.NoError(err)
	assert.Equal("command inbox fixture", text)
	secondSource, err := st.GetSourceByTypeAndIdentifier(meta.SourceTypeFacebookMessages, "app:9:page:101")
	require.NoError(err)
	secondRows, err := st.MessageExistsBatch(secondSource.ID, []string{"m_2"})
	require.NoError(err)
	require.NotZero(secondRows["m_2"])
	assert.Equal(2, strings.Count(output.String(), "messages_added=1"))
	assert.Equal(1, strings.Count(output.String(), "backfill covers at most the 20 most recent messages per thread"))
}

func TestMetaMessagesGroupedRegistration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, provider := range []string{"facebook-messages", "instagram-messages"} {
		for _, registered := range rootCmd.Commands() {
			assert.NotEqual("add-"+provider, registered.Name())
			assert.NotEqual("sync-"+provider, registered.Name())
		}
		for _, operation := range []string{"add", "sync"} {
			path := []string{"source", provider, operation}
			command, rest, err := rootCmd.Find(path)
			require.NoError(err)
			assert.Empty(rest)
			assert.Equal("msgvault source "+provider+" "+operation, command.CommandPath())
			assert.Empty(command.Aliases)
		}
	}
}
func TestMetaMessagesProfileErrorClasses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, tc := range []struct {
		args []string
		want int
	}{{nil, 5}, {[]string{"missing"}, 4}} {
		_, err := resolveMetaMessagesProfiles(meta.PlatformMessenger, tc.args, &config.Config{})
		require.Error(err)
		var classified interface {
			error
			ExitCode() int
			Hint() string
		}
		require.ErrorAs(err, &classified)
		assert.Equal(tc.want, classified.ExitCode())
		assert.NotEmpty(classified.Hint())
	}
}

// The standalone leaf tests keep the production ancestry so daemon forwarding
// observes exactly the public command path.
func messagesTestCommand() *cobra.Command {
	root := &cobra.Command{Use: "msgvault"}
	registerMetaMessagesCommands(root)
	return root
}

func TestMetaMessagesInvocationErrorClasses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"source", "facebook-messages", "add", "one", "two"}, 2},
		{[]string{"source", "instagram-messages", "sync", "--max-requests=0"}, 3},
	} {
		command := messagesTestCommand()
		command.SetArgs(tc.args)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		err := command.Execute()
		var classified interface {
			error
			ExitCode() int
			Hint() string
		}
		require.ErrorAs(err, &classified)
		assert.Equal(tc.want, classified.ExitCode())
		assert.NotEmpty(classified.Hint())
	}
}
