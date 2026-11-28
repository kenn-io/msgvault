package cmd

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAddSlackBrowserSessionForwardsCredentialsFromFiles(t *testing.T) {
	t.Setenv(clirun.EnvSlackToken, "")
	t.Setenv(clirun.EnvSlackCookie, "")
	dir := t.TempDir()
	tokenFile, cookieFile := filepath.Join(dir, "token"), filepath.Join(dir, "cookie")
	require.NoError(t, os.WriteFile(tokenFile, []byte("xoxc-synthetic\n"), 0o600))
	require.NoError(t, os.WriteFile(cookieFile, []byte("xoxd-synthetic%2Fcookie\n"), 0o600))
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, "xoxc-synthetic", req.Env[clirun.EnvSlackToken])
		assert.Equal(t, "xoxd-synthetic%2Fcookie", req.Env[clirun.EnvSlackCookie])
		assert.True(t, clirun.EnvAllowed(clirun.EnvSlackCookie))
	}, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	cmd := newAddSlackCmd()
	cmd.SetContext(ctx)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--token-file", tokenFile, "--cookie-file", cookieFile})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), requests.Load())
	assert.NotContains(t, output.String(), "xoxc-synthetic")
	assert.NotContains(t, output.String(), "xoxd-synthetic")
}

func TestAddSlackRejectsIncompleteBrowserSessionBeforeDaemon(t *testing.T) {
	t.Setenv(clirun.EnvSlackToken, "xoxc-synthetic")
	t.Setenv(clirun.EnvSlackCookie, "")
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	cmd := newAddSlackCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs(nil)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.ErrorContains(t, cmd.Execute(), "requires its d cookie")
	assert.Zero(t, requests.Load())
}

func TestAddSlackBrowserSessionPersistsAndRestoresCookie(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(clirun.EnvSlackToken, "xoxc-synthetic")
	t.Setenv(clirun.EnvSlackCookie, "xoxd-synthetic")
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	require.NoError(t, st.Close())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/auth.test", r.URL.Path)
		assert.Equal(t, "Bearer xoxc-synthetic", r.Header.Get("Authorization"))
		assert.Equal(t, "d=xoxd-synthetic", r.Header.Get("Cookie"))
		_, err := io.WriteString(w, `{"ok":true,"url":"https://synthetic.slack.com/","team":"Synthetic","team_id":"T_SYNTHETIC","user_id":"U_SYNTHETIC"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	cmd := newAddSlackCmdWithClientFactory(func(credentials slack.Credentials) (*slack.Client, error) {
		return slack.NewClientWithCredentials(server.URL, credentials)
	})
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(nil)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "Added Slack workspace Synthetic")
	assert.NotContains(t, output.String(), "xoxc-synthetic")
	assert.NotContains(t, output.String(), "xoxd-synthetic")

	credentials, err := slack.LoadCredentials(cfg.TokensDir(), "T_SYNTHETIC", "U_SYNTHETIC")
	require.NoError(t, err)
	assert.Equal(t, slack.Credentials{Token: "xoxc-synthetic", Cookie: "xoxd-synthetic"}, credentials)
	client, err := slack.LoadClient(server.URL, cfg.TokensDir(), "T_SYNTHETIC", "U_SYNTHETIC")
	require.NoError(t, err)
	_, err = client.AuthTest(t.Context())
	require.NoError(t, err)
}
