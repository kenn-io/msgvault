package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFastmailCredentialSources(t *testing.T) {
	cfg := &Config{}
	cfg.Data.DataDir = t.TempDir()
	require.NoError(t, os.MkdirAll(cfg.TokensDir(), 0700))
	path := filepath.Join(cfg.TokensDir(), "fastmail")
	require.NoError(t, os.WriteFile(path, []byte("synthetic-file-token\n"), 0600))
	t.Setenv("MSGVAULT_TEST_FASTMAIL_TOKEN", "synthetic-env-token")
	t.Setenv("MSGVAULT_TEST_FASTMAIL_EMPTY", "")
	for _, tt := range []struct {
		name    string
		source  FastmailSource
		want    string
		wantErr string
	}{
		{"inline", FastmailSource{APIToken: "synthetic-inline-token"}, "synthetic-inline-token", ""},
		{"environment", FastmailSource{APITokenEnv: "MSGVAULT_TEST_FASTMAIL_TOKEN"}, "synthetic-env-token", ""},
		{"file", FastmailSource{APITokenFile: "fastmail"}, "synthetic-file-token", ""},
		{"missing env", FastmailSource{APITokenEnv: "MSGVAULT_TEST_FASTMAIL_UNSET"}, "", "empty or unset"},
		{"empty env", FastmailSource{APITokenEnv: "  MSGVAULT_TEST_FASTMAIL_EMPTY  "}, "", "empty or unset"},
		{"missing file", FastmailSource{APITokenFile: "missing"}, "", "read Fastmail token file"},
		{"no fallback", FastmailSource{APIToken: "secret-do-not-print", APITokenEnv: "MSGVAULT_TEST_FASTMAIL_UNSET"}, "", "exactly one"},
		{"outside tokens", FastmailSource{APITokenFile: "../outside"}, "", "tokens directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			got, err := cfg.FastmailAPIToken(tt.source)
			if tt.wantErr != "" {
				require.ErrorContains(err, tt.wantErr)
				if tt.name == "missing env" {
					assert.Contains(err.Error(), `"MSGVAULT_TEST_FASTMAIL_UNSET"`)
				}
				if tt.name == "empty env" {
					assert.Contains(err.Error(), `"MSGVAULT_TEST_FASTMAIL_EMPTY"`)
				}
				assert.NotContains(err.Error(), "secret-do-not-print")
				return
			}
			require.NoError(err)
			assert.Equal(tt.want, got)
		})
	}
	require := require.New(t)
	if runtime.GOOS != "windows" {
		require.NoError(os.Chmod(path, 0400))
		got, err := cfg.FastmailAPIToken(FastmailSource{APITokenFile: "fastmail"})
		require.NoError(err, "owner-only read access is private enough")
		assert.Equal(t, "synthetic-file-token", got)
		require.NoError(os.Chmod(path, 0640))
		_, err = cfg.FastmailAPIToken(FastmailSource{APITokenFile: "fastmail"})
		require.ErrorContains(err, "chmod 600")
	} else {
		require.NoError(os.Chmod(path, 0644))
		got, err := cfg.FastmailAPIToken(FastmailSource{APITokenFile: "fastmail"})
		require.NoError(err, "Windows has no mode bits to check")
		assert.Equal(t, "synthetic-file-token", got)
	}
}

func TestFastmailConfigCredentialSelection(t *testing.T) {
	for _, source := range []string{`api_token_env = "FASTMAIL_TOKEN"`, `api_token_file = "fastmail"`} {
		cfg := loadConfigText(t, "[[fastmail]]\nsource_id = 1\n"+source)
		require.Len(t, cfg.Fastmail, 1)
	}
	err := loadConfigTextError(t, "[[fastmail]]\nsource_id = 1\napi_token_env = \"TOKEN\"\napi_token_file = \"file\"")
	require.ErrorContains(t, err, "exactly one")
}

func TestFastmailCredentialFileCannotEscapeTokensThroughSymlink(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may need extra Windows privileges")
	}
	cfg := &Config{}
	cfg.Data.DataDir = t.TempDir()
	require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
	outside := filepath.Join(t.TempDir(), "outside-token")
	require.NoError(os.WriteFile(outside, []byte("synthetic-outside-token"), 0600))
	require.NoError(os.Symlink(outside, filepath.Join(cfg.TokensDir(), "escape")))
	_, err := cfg.FastmailAPIToken(FastmailSource{APITokenFile: "escape"})
	require.Error(err)
	assert.NotContains(t, err.Error(), "synthetic-outside-token")
}
