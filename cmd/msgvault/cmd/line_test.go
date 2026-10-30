package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

const wiringBot = "U00000000000000000000000000000001"

func lineWiringFixture(t *testing.T) (*config.Config, *store.Store, *lineapi.Server) {
	t.Helper()
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_LINE_SECRET", "synthetic-secret")
	t.Setenv("OTHER_LINE_SECRET", "other-synthetic-secret")
	t.Setenv(lineActiveChannelsEnv, "")
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	require.NoError(t, cfg.Save())
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	fake := lineapi.New()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	prior := lineClientFactory
	lineClientFactory = func(id, secret string) *line.Client {
		return line.NewClient(id, secret, line.WithBaseURLs(server.URL, server.URL))
	}
	t.Cleanup(func() { lineClientFactory = prior })
	return cfg, st, fake
}

func executeLineCommand(t *testing.T, cfg *config.Config, command *cobra.Command, args ...string) (string, error) {
	t.Helper()
	command.SetContext(withInvocation(t.Context(), testInvocationWithConfig(cfg)))
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

func TestLineRegistrationTwoChannelsAndActiveRestart(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg, st, fake := lineWiringFixture(t)
	args := []string{"--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com"}
	output, err := executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "restart")
	checks.Contains(output, "Use webhook")
	checks.Contains(output, "Webhook redelivery")
	checks.Zero(fake.Count("PUT", "/v2/bot/channel/webhook/endpoint"))
	checks.Zero(fake.Count("POST", "/v2/bot/channel/webhook/test"))
	_, err = executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000002", "--secret-env", "OTHER_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	must.Len(loaded.Line, 2)
	checks.Equal("OTHER_LINE_SECRET", loaded.GetLineSource("2000000002").SecretEnv)
	loaded.Sync.RateLimitQPS = 17
	must.NoError(loaded.Save())
	// The original cfg remains stale; a repeated add must load the current file.
	output, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "restart")
	checks.Zero(fake.Count("PUT", "/v2/bot/channel/webhook/endpoint"))
	source, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
	must.NoError(err)
	snapshot, err := json.Marshal([]line.ActiveChannel{{ChannelID: "2000000001", SourceID: source.ID, BotUserID: wiringBot, SecretEnv: "EXAMPLE_LINE_SECRET"}})
	must.NoError(err)
	t.Setenv(lineActiveChannelsEnv, string(snapshot))
	output, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "verified")
	checks.Equal(1, fake.Count("PUT", "/v2/bot/channel/webhook/endpoint"))
	checks.Equal(1, fake.Count("POST", "/v2/bot/channel/webhook/test"))
	loaded, err = config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.Len(loaded.Line, 2)
	checks.Equal(17, loaded.Sync.RateLimitQPS)
	// A changed secret variable requires a new mounted receiver snapshot.
	args[3] = "OTHER_LINE_SECRET"
	output, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "restart")
	checks.Equal(1, fake.Count("PUT", "/v2/bot/channel/webhook/endpoint"))
	checks.NotContains(output, "synthetic-secret")
	loaded, err = config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	for index := range loaded.Line {
		if loaded.Line[index].ChannelID == "2000000001" {
			loaded.Line[index].Enabled = false
		}
	}
	must.NoError(loaded.Save())
	t.Setenv(lineActiveChannelsEnv, "")
	output, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "restart")
	loaded, err = config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.False(loaded.GetLineSource("2000000001").Enabled)
}

func TestLineWebhookFailurePreservesRegistration(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg, st, fake := lineWiringFixture(t)
	args := []string{"--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com"}
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
	must.NoError(err)
	snapshot, err := json.Marshal([]line.ActiveChannel{{ChannelID: "2000000001", SourceID: source.ID, BotUserID: wiringBot, SecretEnv: "EXAMPLE_LINE_SECRET"}})
	must.NoError(err)
	t.Setenv(lineActiveChannelsEnv, string(snapshot))
	for _, response := range []lineapi.Response{
		{Status: 200, Body: `{"success":false,"reason":"synthetic-secret"}`},
		{Status: 500, Body: `synthetic-secret`},
	} {
		fake.Enqueue("POST", "/v2/bot/channel/webhook/test", response)
		output, err := executeLineCommand(t, cfg, newAddLineCmd(), args...)
		must.Error(err)
		checks.NotContains(err.Error(), "synthetic-secret")
		checks.NotContains(output, "synthetic-secret")
		loaded, err := config.Load(cfg.ConfigFilePath(), "")
		must.NoError(err)
		checks.NotNil(loaded.GetLineSource("2000000001"))
	}
}

func TestLineCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--channel-id", "01", "--secret-env", "EXAMPLE_LINE_SECRET"},
		{"--channel-id", "1", "--secret-env", "BAD-NAME"},
		{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "http://archive.example.com"},
		{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://user:secret@archive.example.com"},
		{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com/prefix"},
		{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com?query"},
		{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com#fragment"},
	} {
		cfg, _, fake := lineWiringFixture(t)
		_, err := executeLineCommand(t, cfg, newAddLineCmd(), args...)
		require.Error(t, err)
		assert.Zero(t, fake.Count("POST", "/oauth2/v3/token"))
	}
	cfg, _, _ := lineWiringFixture(t)
	t.Setenv("EXAMPLE_LINE_SECRET", "")
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	require.ErrorContains(t, err, "EXAMPLE_LINE_SECRET")
}

func TestLineRuntimeChildEnvAuthoritativeSnapshot(t *testing.T) {
	cfg, st, _ := lineWiringFixture(t)
	receiver, err := line.NewReceiver(st, []line.ReceiverChannel{{ChannelID: "2000000001", SourceID: 1, BotUserID: wiringBot, SecretEnv: "EXAMPLE_LINE_SECRET", Secret: "synthetic-secret"}}, nil)
	require.NoError(t, err)
	t.Setenv(lineActiveChannelsEnv, `[{"channel_id":"forged-inherited"}]`)
	for _, active := range []*line.Receiver{receiver, nil} {
		adapter := &storeAPIAdapter{config: cfg, lineWebhook: active}
		err := adapter.runCLICommandWithRunner(testInvocationContext(t.Context(), cfg, invocationOptions{}), api.CLIRunRequest{Args: []string{"add-line"}, Env: map[string]string{lineActiveChannelsEnv: `[{"channel_id":"forged-request"}]`, strings.ToLower(lineActiveChannelsEnv): "forged-case"}}, nil,
			func(ctx context.Context, args []string, env map[string]string, cwd string, emit func(string, string) error) error {
				command, err := newDaemonCLISubprocessCommand(ctx, args, env, cwd)
				require.NoError(t, err)
				var snapshot string
				count := 0
				for _, entry := range command.Env {
					name, value, ok := strings.Cut(entry, "=")
					if ok && strings.EqualFold(name, lineActiveChannelsEnv) {
						count++
						snapshot = value
					}
				}
				assert.Equal(t, 1, count)
				assert.NotContains(t, snapshot, "forged")
				assert.NotContains(t, snapshot, "synthetic-secret")
				var records []line.ActiveChannel
				require.NoError(t, json.Unmarshal([]byte(snapshot), &records))
				if active == nil {
					assert.Empty(t, records)
				} else {
					assert.Equal(t, active.ActiveChannels(), records)
				}
				return nil
			})
		require.NoError(t, err)
	}
	// Even a call without invocation state clears inherited and request claims.
	env := daemonRuntimeChildEnv(context.Background(), []string{lineActiveChannelsEnv + "=forged"})
	assert.Contains(t, env, lineActiveChannelsEnv+"=[]")
}

func TestLineSyncDrainsConfiguredDisabledChannel(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg, st, _ := lineWiringFixture(t)
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.NoError(err)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	loaded.Line[0].Enabled = false
	must.NoError(loaded.Save())
	source, err := st.GetSourceByTypeAndIdentifier("line", "2000000001")
	must.NoError(err)
	botID, err := st.LineBotUserIDContext(t.Context(), source.ID)
	must.NoError(err)
	must.NoError(st.EnqueueLineEventsContext(t.Context(), source.ID, source.Identifier, botID, []store.LineWebhookEvent{{WebhookEventID: "future-event", EventType: "future", EventTimestamp: 123, RawJSON: `{"type":"future","webhookEventId":"future-event","timestamp":123}`}}))
	output, err := executeLineCommand(t, loaded, addManualSyncCacheFlags(newSyncLineCmd()), "--channel-id", "2000000001", "--no-build-cache")
	must.NoError(err)
	checks.Contains(output, "Push source")
	checks.Contains(output, "pending: 0")
	checks.Contains(output, "failed: 0")
	_, err = executeLineCommand(t, loaded, newSyncLineCmd(), "--channel-id", "999")
	must.Error(err)
	checks.True(manualSyncCLICommand([]string{"sync-line"}))
	checks.True(attachmentProducingCommand([]string{"sync-line"}))
}

func TestLineScheduledCacheProgress(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	prior := rebuildLineCacheAfterScheduledSync
	t.Cleanup(func() { rebuildLineCacheAfterScheduledSync = prior })
	calls := 0
	rebuildLineCacheAfterScheduledSync = func(ctx context.Context, name string) error {
		calls++
		require.NoError(t, ctx.Err())
		assert.Equal(t, "line:2000000001", name)
		return nil
	}
	must.NoError(runLineProcessor(t.Context(), "2000000001", func(context.Context) error { return nil }))
	checks.Zero(calls)
	ctx, cancel := context.WithCancel(t.Context())
	err := runLineProcessor(ctx, "2000000001", func(ctx context.Context) error { jobctx.RecordProgress(ctx); cancel(); return context.Canceled })
	must.ErrorIs(err, context.Canceled)
	checks.Equal(1, calls)
}

func TestLineReadinessRequiresExactParentIdentity(t *testing.T) {
	must := require.New(t)
	lineWiringFixture(t)
	record := line.ActiveChannel{ChannelID: "2000000001", SourceID: 1, BotUserID: wiringBot, SecretEnv: "EXAMPLE_LINE_SECRET"}
	for _, mutate := range []func(*line.ActiveChannel){
		func(r *line.ActiveChannel) { r.ChannelID = "2000000002" }, func(r *line.ActiveChannel) { r.SourceID = 2 }, func(r *line.ActiveChannel) { r.BotUserID = "U00000000000000000000000000000003" }, func(r *line.ActiveChannel) { r.SecretEnv = "OTHER_LINE_SECRET" },
	} {
		changed := record
		mutate(&changed)
		raw, err := json.Marshal([]line.ActiveChannel{changed})
		must.NoError(err)
		t.Setenv(lineActiveChannelsEnv, string(raw))
		assert.False(t, lineReceiverReady(1, record.ChannelID, record.BotUserID, record.SecretEnv))
	}
	raw, err := json.Marshal([]line.ActiveChannel{record})
	must.NoError(err)
	t.Setenv(lineActiveChannelsEnv, string(raw))
	t.Setenv(daemonCLISubprocessEnv, "")
	assert.False(t, lineReceiverReady(1, record.ChannelID, record.BotUserID, record.SecretEnv), "ordinary CLI cannot trust a supplied snapshot")
}

func TestLineNormalCLIUsesDaemonTransport(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	t.Setenv(daemonCLISubprocessEnv, "")
	server, requests := newDaemonCLIRunnerTestServer(t, func(request daemonCLIRunTestRequest) {
		assert.Equal(t, "add-line", request.Args[0])
		assert.Contains(t, request.Args, "--channel-id=2000000001")
		assert.Contains(t, request.Args, "--secret-env=EXAMPLE_LINE_SECRET")
		assert.Empty(t, request.Env, "secret values remain in daemon environment")
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	cfg := invocationFromContext(ctx).cfg
	cfg.Data.DataDir = t.TempDir()
	command := newAddLineCmd()
	command.SetContext(ctx)
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET"})
	must.NoError(command.Execute())
	checks.Equal(int32(1), requests.Load())
	checks.NoFileExists(cfg.DatabaseDSN())
	for _, name := range []string{"add-line", "sync-line"} {
		leaf, _, err := rootCmd.Find([]string{name})
		must.NoError(err)
		checks.Equal(name, leaf.Name())
	}
}
