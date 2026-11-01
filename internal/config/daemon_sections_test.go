package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func writeDaemonSectionConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestInvalidDaemonSectionsAreRecordedForClientsAndRejectedElsewhere(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
	for _, tc := range []struct{ name, content, mention string }{
		{"server", "[server]\napi_port = 70000\n", "api_port"},
		{"web", "[web]\ntheme = 'neon'\n", "[web] theme"},
		{"kata", "[integrations.kata]\nendpoint = '://invalid'\n", "[integrations.kata]"},
		{"docbank", "[integrations.docbank]\nattachment_folder = '../escape'\n", "docbank"},
		{"carddav", "[carddav]\nprovider = 'other'\n", "carddav.provider"},
		{"identity scoring", "[people.identity_scoring]\nbatch_size = 0\n", "identity_scoring.batch_size"},
		{"identity scoring key", "[people.identity_scoring]\nbogus = 1\n", "unknown people.identity_scoring config key"},
		{"draft key", "[[imap.drafts]]\nsource_id = 1\nmailbox = 'Drafts'\nbogus = 1\n", "unknown IMAP draft config key"},
		{"fastmail", "[[fastmail]]\naccount = 'me@example.com'\n", "api_token is required"},
		{"api port env", "", "MSGVAULT_API_PORT"},
		{"cors credentials env", "", "MSGVAULT_CORS_CREDENTIALS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			switch tc.name {
			case "api port env":
				t.Setenv("MSGVAULT_API_PORT", "invalid")
			case "cors credentials env":
				t.Setenv("MSGVAULT_CORS_CREDENTIALS", "maybe")
			}
			path := writeDaemonSectionConfig(t, tc.content)

			cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
			require.NoError(err)
			require.ErrorContains(cfg.DaemonConfigError(), tc.mention)

			_, err = Load(path, "")
			require.ErrorContains(err, tc.mention)
		})
	}

	// Snapshot loaders follow the same split: strict for reloads and edits, lenient for client reads.
	require := require.New(t)
	path := writeDaemonSectionConfig(t, "[integrations.kata]\nendpoint = '://invalid'\n")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	_, err = LoadConfigFile(snapshot, "")
	require.ErrorContains(err, "[integrations.kata]")
	lenient, err := LoadClientConfigFile(snapshot, "")
	require.NoError(err)
	require.ErrorContains(lenient.DaemonConfigError(), "[integrations.kata]")
	_, err = lenient.ReloadConfigFile(snapshot)
	require.ErrorContains(err, "[integrations.kata]")
}

func TestClientLoadStaysStrictOutsideDaemonSections(t *testing.T) {
	for _, tc := range []struct{ name, content, env, want string }{
		{name: "toml syntax", content: "[server\n", want: "decode config"},
		{name: "wrong type", content: "[integrations.kata]\nendpoint = 5\n", want: "decode config"},
		{name: "chatwoot key", content: "[[chatwoot]]\nunknown = true\n", want: "unknown Chatwoot config key"},
		{name: "backup", content: "[backup]\nzstd_level = 20\n", want: "[backup] zstd_level"},
		{name: "auto restart", content: "[server]\ndaemon_auto_restart = 'sometimes'\n", want: "daemon_auto_restart"},
		{name: "oauth", content: "[oauth]\nclient_secrets_command = []\n", want: "client_secrets_command"},
		{name: "people sweep", content: "[people.sweep]\nwork_batch_size = -1\n", want: "work_batch_size"},
		{name: "allow insecure env", env: "MSGVAULT_ALLOW_INSECURE", want: "MSGVAULT_ALLOW_INSECURE"},
		{name: "remote insecure env", env: "MSGVAULT_REMOTE_ALLOW_INSECURE", want: "MSGVAULT_REMOTE_ALLOW_INSECURE"},
	} {
		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest // environment overrides are process-wide
			if tc.env != "" {
				t.Setenv(tc.env, "maybe")
			}
			_, err := LoadClientWithOverrides(writeDaemonSectionConfig(t, tc.content), "", RuntimeOverrides{})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestFastmailSourceForReturnsFastmailConfigError(t *testing.T) {
	require := require.New(t)
	path := writeDaemonSectionConfig(t, "[[fastmail]]\naccount = 'me@example.com'\n")
	cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("fastmail", "me@example.com")
	require.NoError(err)
	_, err = cfg.FastmailSourceFor(st, source.ID)
	require.ErrorContains(err, "api_token is required")
}

func TestSaveRefusesFileThatLosesContentWhenReencoded(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"unknown key", "[people.identity_scoring]\nbatch_size = 5\nbatch_siz = 3\n", "batch_siz"},
		{"explicit zero", "[people.identity_scoring]\nbatch_size = 0\n", "identity_scoring.batch_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			path := writeDaemonSectionConfig(t, tc.content)
			cfg, err := LoadClientWithOverrides(path, "", RuntimeOverrides{})
			require.NoError(err)

			err = cfg.Save()
			require.ErrorContains(err, "refusing to save config")
			require.ErrorContains(err, tc.want)
			after, err := os.ReadFile(path)
			require.NoError(err)
			require.Equal(tc.content, string(after))
		})
	}
}

func TestSaveFailsClosedWhenConfigFileCannotBeChecked(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.configPath = t.TempDir() // a directory, so the file cannot be read
	require.ErrorContains(t, cfg.Save(), "refusing to save config")
}
