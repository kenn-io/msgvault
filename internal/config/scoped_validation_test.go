package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadForSections(t *testing.T) {
	for _, tc := range []struct{ section, content string }{
		{"integrations.kata", "[integrations.kata]\nendpoint = '://invalid'"},
		{"integrations.docbank", "[integrations.docbank]\nattachment_folder = '../escape'"},
		{"web", "[web]\ntheme = 'invalid'"},
		{"analytics", "[analytics]\nengine = 'invalid'"},
		{"server", "[server]\napi_port = -1"},
		{"backup", "[backup]\nzstd_level = 20"},
		{"people.identity_scoring", "[people.identity_scoring]\nbatch_size = 0"},
		{"people.identity_scoring", "[people.identity_scoring]\nminimum_probability = 0.0"},
		{"beeper", "[beeper]\nmax_media_mb = -1"},
		{"fastmail", "[[fastmail]]\nsource_id = 0"},
		{"imap.drafts", "[[imap.drafts]]\nsource_id = 0"},
		{"gmail.drafts", "[[gmail.drafts]]\nsource_id = 0"},
		{"beeper.drafts", "[[beeper.drafts]]\nsource_id = 0"},
		{"attachments.documents", "[attachments.documents]\nprovider = 'invalid'"},
		{"vector", "[vector]\nenabled = true"},
		{"oauth", "[oauth]\nclient_secrets_command = []"},
		{"carddav", "[carddav]\nprovider = 'invalid'"},
		{"carddav_connections", "[carddav_connections.default]\nprovider = 'google'"},
		{"integrations.tasks", "[integrations.tasks]\nendpoint = '://invalid'"},
		{"activity", "[activity]\ntimezone = 'Local'"},
		{"people.sweep", "[people.sweep]\nwork_batch_size = -1"},
		{"people.enrichment", "[people.enrichment]\nbatch_size = -1"},
		{"chatwoot", "[[chatwoot]]\nidentifier = 'support'\naccount_id = 0"},
		{"slack", "[slack]\nmax_media_mb = -1"},
		{"discord", "[discord]\nmax_media_mb = -1"},
		{"teams", "[teams]\nmax_media_mb = -1"},
		{"twenty", "[[twenty]]\nidentifier = 'same'\n[[twenty]]\nidentifier = 'same'"},
		{"omi", "[[omi]]\nidentifier = 'same'\n[[omi]]\nidentifier = 'same'"},
		{"granola", "[[granola]]\nidentifier = 'same'\n[[granola]]\nidentifier = 'same'"},
		{"circleback", "[[circleback]]\nidentifier = 'same'\n[[circleback]]\nidentifier = 'same'"},
		{"plaud", "[[plaud]]\nidentifier = 'same'\n[[plaud]]\nidentifier = 'same'"},
		{"notion_meetings", "[[notion_meetings]]\nidentifier = 'same'\n[[notion_meetings]]\nidentifier = 'same'"},
		{"muesli", "[[muesli]]\nidentifier = 'same'\n[[muesli]]\nidentifier = 'same'"},
		{"twilio", "[[twilio]]\nidentifier = 'same'\n[[twilio]]\nidentifier = 'same'"},
	} {
		t.Run(tc.section+tc.content, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			require.NoError(os.WriteFile(path, []byte(tc.content), 0600))
			cfg, err := LoadForSections(path, home, RuntimeOverrides{})
			require.NoError(err)
			require.Error(cfg.ValidateFor(Section(tc.section)))
			warnings := cfg.UnusedSectionErrors()
			require.NotEmpty(warnings)
			assert.Contains(fmt.Sprint(warnings), tc.section)
			_, err = LoadForSections(path, home, RuntimeOverrides{}, Section(tc.section))
			require.Error(err)
			_, err = Load(path, home)
			require.Error(err)
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)
			_, err = LoadConfigFile(snapshot, home)
			require.Error(err)
		})
	}
}

// Any TOML string endpoint in an unused section must leave unrelated commands
// usable. Required-section validation and whole-file validation must agree.
func FuzzScopedConfigEndpoint(f *testing.F) {
	for _, endpoint := range []string{"", "://invalid", "http://example.test", "https://example.test", "\n", "\x00"} {
		f.Add(endpoint)
	}
	f.Fuzz(func(t *testing.T, endpoint string) {
		require := require.New(t)
		assert := assert.New(t)
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		if !utf8.ValidString(endpoint) {
			t.Skip("TOML requires UTF-8")
		}
		var encoded bytes.Buffer
		require.NoError(toml.NewEncoder(&encoded).Encode(map[string]any{"integrations": map[string]any{"kata": map[string]any{"endpoint": endpoint}}}))
		require.NoError(os.WriteFile(path, encoded.Bytes(), 0600))
		cfg, err := LoadForSections(path, home, RuntimeOverrides{})
		require.NoError(err)
		assert.Equal(endpoint, cfg.Integrations.Kata.Endpoint)
		scopedErr := cfg.ValidateFor("integrations.kata")
		_, wholeErr := Load(path, home)
		assert.Equal(scopedErr == nil, wholeErr == nil)
		if scopedErr != nil {
			assert.NotEmpty(cfg.UnusedSectionErrors())
		}
	})
}

func TestScopedRuntimeErrors(t *testing.T) { //nolint:paralleltest // process environment
	for _, tc := range []struct {
		name, value string
		section     Section
	}{
		{"MSGVAULT_API_PORT", "invalid", "server"}, {"MSGVAULT_BIND_ADDR", "", "server"},
		{"MSGVAULT_ALLOW_INSECURE", "invalid", "server"}, {"MSGVAULT_CORS_CREDENTIALS", "invalid", "server"},
		{"MSGVAULT_REMOTE_ALLOW_INSECURE", "invalid", "remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			t.Setenv(tc.name, tc.value)
			cfg, err := LoadForSections("", t.TempDir(), RuntimeOverrides{})
			require.NoError(err)
			require.Error(cfg.ValidateFor(tc.section))
			warnings := cfg.UnusedSectionErrors()
			require.NotEmpty(warnings)
			assert.Contains(fmt.Sprint(warnings), string(tc.section))
		})
	}
}

func TestScopedConfigKeepsExplicitZeroScoringValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[people.identity_scoring]\nminimum_probability = 0\nbatch_size = 0\n"), 0600))
	cfg, err := LoadForSections(path, home, RuntimeOverrides{})
	require.NoError(err)
	assert.Zero(cfg.People.IdentityScoring.MinimumProbability)
	assert.Zero(cfg.People.IdentityScoring.BatchSize)
	assert.NotEmpty(cfg.UnusedSectionErrors())
	assert.Zero(cfg.People.IdentityScoring.MinimumProbability)
	assert.Zero(cfg.People.IdentityScoring.BatchSize)
}

func TestScopedConfigSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	for _, content := range []string{"[web]\ntheme = 'invalid'", "[web\n", "[chatwoot]\nunknown = true"} {
		require.NoError(os.WriteFile(path, []byte(content), 0600))
		snapshot, err := ReadConfigFile(path)
		require.NoError(err)
		cfg, err := LoadConfigFileForSections(snapshot, home)
		if content == "[web]\ntheme = 'invalid'" {
			require.NoError(err)
			assert.Equal("invalid", cfg.Web.Theme)
			assert.NotEmpty(cfg.UnusedSectionErrors())
			_, err = LoadConfigFileForSections(snapshot, home, "web")
			require.Error(err)
		} else {
			require.Error(err)
		}
	}
}
