package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
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
		if err != nil {
			return fmt.Errorf("execute synthetic Threads connection: %w", err)
		}
		return nil
	})
}

type threadsCallbackInput struct {
	output *bytes.Buffer
	reader *strings.Reader
}

func (i *threadsCallbackInput) Read(buffer []byte) (int, error) {
	if i.reader == nil {
		for line := range strings.SplitSeq(i.output.String(), "\n") {
			if strings.HasPrefix(line, "https://threads.com/oauth/authorize?") {
				authorization, err := url.Parse(line)
				if err != nil {
					return 0, fmt.Errorf("parse synthetic authorization URL: %w", err)
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
	n, err := i.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read synthetic callback: %w", err)
	}
	return n, nil
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
	_, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, "threads:123:20")
	require.NoError(err)
}

func TestAddThreadsRequiresMaskedPromptOrExplicitSecretEnvironment(t *testing.T) {
	t.Setenv("MSGVAULT_THREADS_APP_SECRET", "")
	t.Setenv(daemonCLISubprocessEnv, "")
	server, _ := newDaemonCLIRunnerTestServer(t, nil)
	cmd := newAddThreadsCmd(threadsCommandDeps{})
	cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
	cmd.SetIn(strings.NewReader("unmasked-secret\n"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--app-id", "123", "--redirect-uri", "https://example.com/threads-callback"})
	require.ErrorContains(t, cmd.Execute(), "MSGVAULT_THREADS_APP_SECRET")
}

func TestAddThreadsRejectsUnsafeDaemonBeforeAuthorization(t *testing.T) {
	t.Setenv("MSGVAULT_THREADS_APP_SECRET", "")
	t.Setenv(daemonCLISubprocessEnv, "")
	cmd := newAddThreadsCmd(threadsCommandDeps{})
	cmd.SetContext(configureRemoteDaemonForTest(t, "http://localhost:1234"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader("unused\n"))
	cmd.SetArgs([]string{"--app-id", "123", "--redirect-uri", "https://example.com/callback"})
	require.ErrorContains(t, cmd.Execute(), "literal loopback HTTP or authenticated HTTPS")
	assert.NotContains(t, out.String(), "authorization URL")
}
