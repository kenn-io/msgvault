package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/testutil"
)

type threadsConnectDaemon struct {
	*storeAPIAdapter

	deps threadsCommandDeps
}

// The runner executes the production Cobra command in this test process so
// endpoint injection remains a constructor seam, never a production flag.
func (d *threadsConnectDaemon) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	return d.runCLICommandWithRunner(ctx, req, emit, func(ctx context.Context, args []string, env map[string]string, _ string, output func(string, string) error) error {
		deps := d.deps
		deps.oauthPayload = func() string { return env[clirun.EnvThreadsOAuth] }
		cmd := newAddThreadsLocalCmd(deps)
		cmd.SetContext(ctx)
		cmd.SetArgs(args[1:])
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		err := cmd.Execute()
		if stdout.Len() > 0 {
			if writeErr := output("stdout", stdout.String()); writeErr != nil {
				return writeErr
			}
		}
		if stderr.Len() > 0 {
			if writeErr := output("stderr", stderr.String()); writeErr != nil {
				return writeErr
			}
		}
		return err
	})
}

func TestAddThreadsThroughRealDaemonHandler(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	directory := t.TempDir()
	cfg := &config.Config{HomeDir: directory, Data: config.DataConfig{DataDir: directory}, Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	adapter := &threadsConnectDaemon{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg}, deps: provider.deps(st, cfg.TokensDir())}
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(apiServer.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	payload, err := json.Marshal(threadsOAuthPayload{AppID: "123", RedirectURI: "https://example.com/threads-callback", Code: "synthetic-code", AppSecret: "synthetic-app-secret"})
	require.NoError(err)
	var output bytes.Buffer
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"add-threads", "--app-id", "123", "--redirect-uri", "https://example.com/threads-callback"}, Env: map[string]string{clirun.EnvThreadsOAuth: string(payload)}}, func(stream, data string) error {
		if stream == "stdout" {
			_, _ = output.WriteString(data)
		}
		return nil
	})
	require.NoError(err)
	assert.Contains(t, output.String(), "threads:123:20")
	_, err = st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	_, err = adapter.deps.tokenManager().Load("123", "20")
	require.NoError(err)
}

func TestThreadsConnectDirectGrantDeniedBeforeRunner(t *testing.T) {
	adapter := &storeAPIAdapter{}
	calls := 0
	err := adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"add-threads"}, Grant: &agentgrant.Grant{}}, nil, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
		calls++
		return nil
	})
	require.ErrorContains(t, err, "owner")
	assert.Zero(t, calls)
}

type threadsCallbackInput struct {
	output *bytes.Buffer
	reader *strings.Reader
}

func (i *threadsCallbackInput) Read(buffer []byte) (int, error) {
	if i.reader == nil {
		for _, line := range strings.Split(i.output.String(), "\n") {
			if strings.HasPrefix(line, "https://threads.com/oauth/authorize?") {
				authorization, err := url.Parse(line)
				if err != nil {
					return 0, err
				}
				callback := "https://example.com/threads-callback?" + url.Values{"state": {authorization.Query().Get("state")}, "code": {"synthetic-code"}}.Encode()
				i.reader = strings.NewReader(callback + "\n")
				break
			}
		}
		if i.reader == nil {
			return 0, errors.New("callback requested before authorization URL")
		}
	}
	return i.reader.Read(buffer)
}

func TestAddThreadsWorkstationCallbackThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	directory := t.TempDir()
	cfg := &config.Config{HomeDir: directory, Data: config.DataConfig{DataDir: directory}, Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	adapter := &threadsConnectDaemon{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg}, deps: provider.deps(st, cfg.TokensDir())}
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(apiServer.Router())
	t.Cleanup(server.Close)
	clientDir := t.TempDir()
	clientCfg := &config.Config{HomeDir: clientDir, Data: config.DataConfig{DataDir: clientDir}, Remote: config.RemoteConfig{APIKey: cfg.Server.APIKey}}
	ctx := configureRemoteDaemonForTest(t, server.URL, clientCfg)
	t.Setenv("MSGVAULT_THREADS_APP_SECRET", "synthetic-app-secret")
	t.Setenv(daemonCLISubprocessEnv, "")
	cmd := newAddThreadsCmd(threadsCommandDeps{})
	cmd.SetContext(ctx)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetIn(&threadsCallbackInput{output: &stdout})
	cmd.SetArgs([]string{"--app-id", "123", "--redirect-uri", "https://example.com/threads-callback"})
	require.NoError(cmd.Execute())
	assert.Contains(stdout.String(), "threads:123:20")
	assert.NotContains(stdout.String(), "synthetic-app-secret")
	assert.NotContains(stdout.String(), "synthetic-code")
	_, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
}

func TestAddThreadsRequiresMaskedPromptOrExplicitSecretEnvironment(t *testing.T) {
	t.Setenv("MSGVAULT_THREADS_APP_SECRET", "")
	t.Setenv(daemonCLISubprocessEnv, "")
	cmd := newAddThreadsCmd(threadsCommandDeps{})
	cmd.SetIn(strings.NewReader("unmasked-secret\n"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--app-id", "123", "--redirect-uri", "https://example.com/threads-callback"})
	require.ErrorContains(t, cmd.Execute(), "MSGVAULT_THREADS_APP_SECRET")
}
