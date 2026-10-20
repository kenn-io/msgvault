package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func writeDaemonSectionConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestInvalidDaemonSectionsAreRecordedForClientsAndRejectedElsewhere(t *testing.T) {
	for _, tc := range []struct{ name, content, mention string }{
		{"server", "[server]\napi_port = 70000\n", "api_port"},
		{"web", "[web]\ntheme = 'neon'\n", "[web] theme"},
		{"tasks", "[integrations.tasks]\nendpoint = '://invalid'\n", "[integrations.tasks]"},
		{"kata", "[integrations.kata]\nendpoint = '://invalid'\n", "[integrations.kata]"},
		{"docbank", "[integrations.docbank]\nattachment_folder = '../escape'\n", "docbank"},
		{"carddav", "[carddav]\nprovider = 'other'\n", "carddav.provider"},
		{"carddav connections", "[carddav_connections.default]\nprovider = 'google'\n", "carddav_connections"},
		{"identity scoring", "[people.identity_scoring]\nbatch_size = 0\n", "identity_scoring.batch_size"},
		{"imap drafts", "[[imap.drafts]]\nmailbox = 'Drafts'\n", "imap.drafts"},
		{"fastmail", "[[fastmail]]\naccount = 'me@example.com'\n", "api_token is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := writeDaemonSectionConfig(t, tc.content)

			cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
			require.NoError(err)
			require.Len(cfg.DaemonConfigErrors(), 1)
			require.ErrorContains(cfg.DaemonConfigErrors()[0], tc.mention)

			_, err = Load(path, "")
			require.ErrorContains(err, tc.mention)
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)
			_, err = LoadConfigFile(snapshot, "")
			require.ErrorContains(err, tc.mention)
			lenient, err := LoadClientConfigFile(snapshot, "")
			require.NoError(err)
			assert.Len(lenient.DaemonConfigErrors(), 1)
		})
	}
}

func TestInvalidEnvironmentIsRecordedWithoutSkippingTheRest(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_API_PORT", "invalid")
	t.Setenv("MSGVAULT_ALLOW_INSECURE", "maybe")
	t.Setenv("MSGVAULT_CORS_CREDENTIALS", "true")
	cfg, err := LoadClientWithOverrides("", t.TempDir(), RuntimeOverrides{})
	require.NoError(err)
	assert.Len(cfg.DaemonConfigErrors(), 2)
	assert.True(cfg.Server.CORSCredentials)
	assert.False(cfg.Server.AllowInsecure)

	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "maybe")
	_, err = LoadClientWithOverrides("", t.TempDir(), RuntimeOverrides{})
	assert.ErrorContains(err, "MSGVAULT_REMOTE_ALLOW_INSECURE")
}

func TestClientSectionsStayStrictAndDaemonErrorsGuardReaders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, content := range []string{
		"[backup]\nzstd_level = 20\n",
		"[server]\ndaemon_auto_restart = 'sometimes'\n",
		"[oauth]\nclient_secrets_command = []\n",
		"[people.sweep]\nwork_batch_size = -1\n",
	} {
		_, err := LoadClientWithOverrides(writeDaemonSectionConfig(t, content), "", RuntimeOverrides{})
		require.Error(err, content)
	}

	path := writeDaemonSectionConfig(t, "[integrations.kata]\nendpoint = '://invalid'\n[[fastmail]]\naccount = 'me@example.com'\n")
	cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
	require.NoError(err)
	require.Len(cfg.DaemonConfigErrors(), 2)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("fastmail", "me@example.com")
	require.NoError(err)
	_, err = cfg.FastmailSourceFor(st, source.ID)
	require.ErrorContains(err, "api_token is required")

	before, err := os.ReadFile(path)
	require.NoError(err)
	require.ErrorContains(cfg.Save(), "[integrations.kata]")
	after, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(before, after)
}
