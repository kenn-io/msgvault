package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotionUsersTokenReferences(t *testing.T) {
	t.Setenv("MSGVAULT_TEST_NOTION_USERS", "  integration-secret  ")
	path := filepath.Join(t.TempDir(), "users-token")
	require.NoError(t, os.WriteFile(path, []byte("integration-secret\n"), 0600))
	for _, tc := range []struct {
		name    string
		source  NotionMeetingsSource
		want    string
		failure string
	}{
		{"optional", NotionMeetingsSource{}, "", ""},
		{"environment", NotionMeetingsSource{UsersTokenEnv: "MSGVAULT_TEST_NOTION_USERS"}, "integration-secret", ""},
		{"file", NotionMeetingsSource{UsersTokenFile: path}, "integration-secret", ""},
		{"exclusive", NotionMeetingsSource{UsersTokenEnv: "MSGVAULT_TEST_NOTION_USERS", UsersTokenFile: path}, "", "only one"},
		{"unset", NotionMeetingsSource{UsersTokenEnv: "MSGVAULT_TEST_NOTION_MISSING"}, "", "unset or empty"},
		{"missing file", NotionMeetingsSource{UsersTokenFile: path + "-missing"}, "", "read users_token_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			got, err := tc.source.ResolveUsersToken()
			if tc.failure != "" {
				require.ErrorContains(err, tc.failure)
				assert.NotContains(err.Error(), "integration-secret")
				return
			}
			require.NoError(err)
			assert.Equal(tc.want, got)
		})
	}
}

func TestLoadNotionUserCredentialReference(t *testing.T) {
	require := require.New(t)
	path := writeMeetingConfig(t, `[[notion_meetings]]
account_email = "owner@example.com"
token = "pat-example"
users_token_file = "secrets/users-token"
`)
	cfg, err := Load(path, "")
	require.NoError(err)
	require.Len(cfg.NotionMeetings, 1)
	assert.Equal(t, filepath.Join(filepath.Dir(path), "secrets/users-token"), cfg.NotionMeetings[0].UsersTokenFile)
	// Loading config keeps the reference, even before the secret file exists.
	_, err = cfg.NotionMeetings[0].ResolveUsersToken()
	require.ErrorContains(err, "read users_token_file")
}

func TestLoadDefaultNotionUserCredentialReference(t *testing.T) {
	require := require.New(t)
	home := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[[notion_meetings]]
account_email = "owner@example.com"
token = "pat-example"
users_token_file = "secrets/users-token"
`), 0600))
	cfg, err := Load("", home)
	require.NoError(err)
	require.Len(cfg.NotionMeetings, 1)
	assert.Equal(t, filepath.Join(home, "secrets/users-token"), cfg.NotionMeetings[0].UsersTokenFile)
}
