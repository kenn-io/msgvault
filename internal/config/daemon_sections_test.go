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
		{"carddav password", "[carddav]\npassword = 'secret'\n", "[carddav] password is not allowed"},
		{"carddav connection password", "[carddav_connections.work]\npassword = 'secret'\n", "carddav_connections passwords"},
		{"identity scoring key", "[people.identity_scoring]\nbogus = 1\n", "unknown people.identity_scoring config key"},
		{"imap draft key", "[[imap.drafts]]\nsource_id = 1\nmailbox = 'Drafts'\nbogus = 1\n", "unknown IMAP draft config key"},
		{"gmail draft key", "[[gmail.drafts]]\nsource_id = 1\nbogus = 1\n", "unknown Gmail draft config key"},
		{"beeper draft key", "[[beeper.drafts]]\nsource_id = 1\nbogus = 1\n", "unknown Beeper draft config key"},
		{"fastmail", "[[fastmail]]\naccount = 'me@example.com'\n", "api_token is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			path := writeDaemonSectionConfig(t, tc.content)

			cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
			require.NoError(err)
			require.Len(cfg.DaemonConfigErrors(), 1)
			require.ErrorContains(cfg.DaemonConfigErrors()[0], tc.mention)

			_, err = Load(path, "")
			require.ErrorContains(err, tc.mention)
		})
	}

	// Snapshot loaders follow the same split: strict for edits, lenient for client reads.
	require := require.New(t)
	snapshot, err := ReadConfigFile(writeDaemonSectionConfig(t, "[integrations.kata]\nendpoint = '://invalid'\n"))
	require.NoError(err)
	_, err = LoadConfigFile(snapshot, "")
	require.ErrorContains(err, "[integrations.kata]")
	lenient, err := LoadClientConfigFile(snapshot, "")
	require.NoError(err)
	require.Len(lenient.DaemonConfigErrors(), 1)
}

func TestInvalidEnvironmentIsRecordedWithoutSkippingTheRest(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("MSGVAULT_API_PORT", "invalid")
	t.Setenv("MSGVAULT_ALLOW_INSECURE", "maybe")
	t.Setenv("MSGVAULT_CORS_CREDENTIALS", "true")
	cfg, err := LoadClientWithOverrides("", t.TempDir(), RuntimeOverrides{})
	require.NoError(err)
	require.Len(cfg.DaemonConfigErrors(), 2)
	require.ErrorContains(cfg.DaemonConfigErrors()[0], "MSGVAULT_API_PORT")
	require.ErrorContains(cfg.DaemonConfigErrors()[1], "MSGVAULT_ALLOW_INSECURE")
	assert.True(cfg.Server.CORSCredentials)
	assert.False(cfg.Server.AllowInsecure)
}

func TestClientLoadStaysStrictOutsideDaemonSections(t *testing.T) {
	for _, tc := range []struct{ name, content, env, want string }{
		{name: "toml syntax", content: "[server\n", want: "decode config"},
		{name: "chatwoot key", content: "[[chatwoot]]\nunknown = true\n", want: "unknown Chatwoot config key"},
		{name: "backup", content: "[backup]\nzstd_level = 20\n", want: "[backup] zstd_level"},
		{name: "auto restart", content: "[server]\ndaemon_auto_restart = 'sometimes'\n", want: "daemon_auto_restart"},
		{name: "oauth", content: "[oauth]\nclient_secrets_command = []\n", want: "client_secrets_command"},
		{name: "people sweep", content: "[people.sweep]\nwork_batch_size = -1\n", want: "work_batch_size"},
		{name: "remote env", env: "maybe", want: "MSGVAULT_REMOTE_ALLOW_INSECURE"},
	} {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
			if tc.env != "" {
				t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", tc.env)
			}
			_, err := LoadClientWithOverrides(writeDaemonSectionConfig(t, tc.content), "", RuntimeOverrides{})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestFastmailSourceForReturnsFastmailConfigError(t *testing.T) {
	require := require.New(t)
	path := writeDaemonSectionConfig(t, "[integrations.kata]\nendpoint = '://invalid'\n[[fastmail]]\naccount = 'me@example.com'\n")
	cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("fastmail", "me@example.com")
	require.NoError(err)
	_, err = cfg.FastmailSourceFor(st, source.ID)
	require.ErrorContains(err, "api_token is required")
}

func TestSaveRefusesConfigWithRecordedDaemonErrors(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	for _, tc := range []struct{ name, content, env, want string }{
		{name: "unknown key", content: "[people.identity_scoring]\nbatch_size = 5\nbatch_siz = 3\n", want: "batch_siz"},
		{name: "explicit zero", content: "[people.identity_scoring]\nbatch_size = 0\n", want: "identity_scoring.batch_size"},
		{name: "environment", env: "invalid", want: "MSGVAULT_API_PORT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			if tc.env != "" {
				t.Setenv("MSGVAULT_API_PORT", tc.env)
			}
			path := writeDaemonSectionConfig(t, tc.content)
			cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
			require.NoError(err)
			before, err := os.ReadFile(path)
			require.NoError(err)

			err = cfg.Save()
			require.ErrorContains(err, "refusing to save config")
			require.ErrorContains(err, tc.want)
			after, err := os.ReadFile(path)
			require.NoError(err)
			require.Equal(before, after)
		})
	}
}
