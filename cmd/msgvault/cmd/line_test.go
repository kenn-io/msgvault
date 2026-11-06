package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestLineRegistrationPreservesSettingsAndPrintsSetup(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg, _, _ := lineWiringFixture(t)
	args := []string{"--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com"}
	output, err := executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "Restart")
	checks.Contains(output, "Use webhook")
	checks.Contains(output, "Webhook redelivery")
	checks.NotContains(output, "Webhook route:")
	output, err = executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000002", "--secret-env", "OTHER_LINE_SECRET")
	must.NoError(err)
	checks.Contains(output, "Webhook route: /webhooks/line/2000000002")
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	must.Len(loaded.Line, 2)
	checks.Equal("OTHER_LINE_SECRET", loaded.GetLineSource("2000000002").SecretEnv)
	output, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	checks.Contains(output, "https://archive.example.com/webhooks/line/2000000001")
	loaded, err = config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.Len(loaded.Line, 2)
	// A changed secret variable takes effect when the daemon restarts.
	args[3] = "OTHER_LINE_SECRET"
	_, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	loaded, err = config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.Equal("OTHER_LINE_SECRET", loaded.GetLineSource("2000000001").SecretEnv)
	for index := range loaded.Line {
		if loaded.Line[index].ChannelID == "2000000001" {
			loaded.Line[index].Enabled = false
		}
	}
	must.NoError(loaded.Save())
	_, err = executeLineCommand(t, cfg, newAddLineCmd(), args...)
	must.NoError(err)
	loaded, err = config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.False(loaded.GetLineSource("2000000001").Enabled)
}

func TestLineCommandValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--channel-id", "01", "--secret-env", "EXAMPLE_LINE_SECRET"}, "--channel-id: 1–18446744073709551615, no leading zeros"},
		{[]string{"--channel-id", "1", "--secret-env", "BAD-NAME"}, "--secret-env must name an explicit environment variable"},
		{[]string{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "http://archive.example.com"}, "--webhook-base-url must be an HTTPS origin"},
		{[]string{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://user:secret@archive.example.com"}, "--webhook-base-url must be an HTTPS origin"},
		{[]string{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com/prefix"}, "--webhook-base-url must be an HTTPS origin"},
		{[]string{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com?query"}, "--webhook-base-url must be an HTTPS origin"},
		{[]string{"--channel-id", "1", "--secret-env", "EXAMPLE_LINE_SECRET", "--webhook-base-url", "https://archive.example.com#fragment"}, "--webhook-base-url must be an HTTPS origin"},
	} {
		cfg, _, fake := lineWiringFixture(t)
		_, err := executeLineCommand(t, cfg, newAddLineCmd(), tc.args...)
		require.ErrorContains(t, err, tc.want)
		assert.Zero(t, fake.Count("POST", "/oauth2/v3/token"))
	}
	cfg, _, _ := lineWiringFixture(t)
	t.Setenv("EXAMPLE_LINE_SECRET", "")
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	require.ErrorContains(t, err, "EXAMPLE_LINE_SECRET")
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
	events := make([]store.LineWebhookEvent, 501)
	for i := range events {
		id := fmt.Sprintf("message-%03d", i)
		raw := fmt.Sprintf(`{"type":"message","webhookEventId":%q,"timestamp":123,"source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":%q,"type":"text","text":"synthetic"}}`, id, id)
		events[i] = store.LineWebhookEvent{WebhookEventID: id, EventType: "message", TargetMessageID: id, EventTimestamp: 123, RawJSON: raw}
	}
	must.NoError(st.EnqueueLineEventsContext(t.Context(), source.ID, source.Identifier, events))
	output, err := executeLineCommand(t, loaded, addManualSyncCacheFlags(newSyncLineCmd()), "--channel-id", "2000000001", "--no-build-cache")
	must.NoError(err)
	checks.Contains(output, "Processing due LINE events; no history is fetched.")
	checks.Contains(output, "pending: 0")
	checks.Contains(output, "failed: 0")
	var archived int
	must.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_id=?", source.ID).Scan(&archived))
	checks.Equal(501, archived)
	_, err = executeLineCommand(t, loaded, newSyncLineCmd(), "--channel-id", "999")
	must.Error(err)
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
}

func TestAddLinePreservesConcurrentConfigEdit(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	cfg, _, fake := lineWiringFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/bot/info" {
			snapshot, err := config.ReadConfigFile(cfg.ConfigFilePath())
			if !checks.NoError(err) {
				http.Error(w, "config edit failed", http.StatusInternalServerError)
				return
			}
			_, err = config.EditConfigFile(cfg.ConfigFilePath(), snapshot.ETag, []config.Edit{{Key: "sync.rate_limit_qps", Value: 17}})
			if !checks.NoError(err) {
				http.Error(w, "config edit failed", http.StatusInternalServerError)
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	lineClientFactory = func(id, secret string) *line.Client {
		return line.NewClient(id, secret, line.WithBaseURLs(server.URL, server.URL))
	}
	_, err := executeLineCommand(t, cfg, newAddLineCmd(), "--channel-id", "2000000001", "--secret-env", "EXAMPLE_LINE_SECRET")
	must.ErrorIs(err, config.ErrConfigConflict)
	loaded, err := config.Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.Equal(17, loaded.Sync.RateLimitQPS)
}
