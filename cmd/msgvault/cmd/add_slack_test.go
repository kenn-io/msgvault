package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
)

func TestAddSlackContinuesWhenDisplayNameConflictsWithAlias(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("MSGVAULT_SLACK_TOKEN", "xoxp-synthetic-token")

	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	selector, err := st.GetOrCreateSource(sourceTypeGmail, "reader@example.test")
	require.NoError(err)
	alias := "Slack Synthetic"
	_, err = st.UpdateSourceSettingsContext(t.Context(), selector.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.NoError(err)
	require.NoError(st.Close())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/auth.test", r.URL.Path)
		assert.Equal("Bearer xoxp-synthetic-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"ok":true,"url":"https://synthetic.slack.com/","team":"Synthetic","team_id":"T_SYNTHETIC","user":"Reader","user_id":"U_SYNTHETIC"}`))
		assert.NoError(err)
	}))
	defer server.Close()

	cmd := newAddSlackCmdWithClientFactory(func(token string) *slack.Client {
		return slack.NewClient(server.URL, token)
	})
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	cmd.SetContext(ctx)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(nil)

	require.NoError(cmd.Execute(), "a display-name alias conflict must not abort add-slack")
	assert.Contains(output.String(), "Added Slack workspace Synthetic")
	loadedToken, err := slack.LoadToken(cfg.TokensDir(), "T_SYNTHETIC", "U_SYNTHETIC")
	require.NoError(err)
	assert.Equal("xoxp-synthetic-token", loadedToken)

	st, err = store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { assert.NoError(st.Close()) }()
	source, err := st.GetSourceByTypeAndIdentifier(sourceTypeSlack, "T_SYNTHETIC:U_SYNTHETIC")
	require.NoError(err)
	assert.False(source.DisplayName.Valid, "the conflicting display name should keep its prior empty value")
	var identityCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM account_identities WHERE source_id = ?`), source.ID).Scan(&identityCount))
	assert.Equal(1, identityCount, "source setup must continue after the display name is skipped")
}
