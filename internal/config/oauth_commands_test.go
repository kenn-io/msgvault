package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthCommandConfiguration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[oauth]
client_secrets_command = ["secret-store", "literal $HOME; value"]
[oauth.apps.work]
client_secrets_command = ["secret-store", "work"]
[oauth.tokens]
read_command = ["secret-store", "read"]
write_command = ["secret-store", "write"]
delete_command = ["secret-store", "delete"]
list_command = ["secret-store", "list"]
`), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.True(cfg.OAuth.HasAnyConfig())
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	assert.Equal([]string{"secret-store", "literal $HOME; value"}, source.ClientSecretsCommand)
	named, err := cfg.OAuth.CredentialsFor("work")
	require.NoError(err)
	assert.Equal([]string{"secret-store", "work"}, named.ClientSecretsCommand)
	_, err = cfg.OAuth.CredentialsFor("missing")
	require.Error(err)
	require.NoError(cfg.Save())
	loaded, err := Load(path, "")
	require.NoError(err)
	assert.Equal(cfg.OAuth, loaded.OAuth)
}

func TestOAuthCommandConfigurationRejectsInvalid(t *testing.T) {
	for _, data := range []string{
		`[oauth]
client_secrets="secret.json"
client_secrets_command=["reader"]`,
		`[oauth.apps.work]
client_secrets="secret.json"
client_secrets_command=["reader"]`,
		`[oauth]
client_secrets_command=[]`,
		`[oauth]
client_secrets_command=[""]`,
		`[oauth.tokens]
read_command=["reader"]`,
		`[oauth]
client_secrets_command=["reader", "\u0000"]`,
	} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(data), 0600))
			_, err := Load(path, "")
			assert.Error(t, err)
		})
	}
}

func TestOAuthTokenCommandsRequireAllFourCommands(t *testing.T) {
	for _, missing := range []string{"read_command", "write_command", "delete_command", "list_command"} {
		t.Run(missing, func(t *testing.T) {
			commands := OAuthTokenCommands{
				ReadCommand: []string{"secret-store", "read"}, WriteCommand: []string{"secret-store", "write"},
				DeleteCommand: []string{"secret-store", "delete"}, ListCommand: []string{"secret-store", "list"},
			}
			switch missing {
			case "read_command":
				commands.ReadCommand = nil
			case "write_command":
				commands.WriteCommand = nil
			case "delete_command":
				commands.DeleteCommand = nil
			case "list_command":
				commands.ListCommand = nil
			}
			require.ErrorContains(t, commands.Validate(), "tokens."+missing+": command is required")
		})
	}
}

// Any NUL in argv must be rejected; other argument bytes are literal data.
func FuzzOAuthCommandValidation(f *testing.F) {
	f.Add("literal $HOME; argument")
	f.Add("\x00")
	f.Add("")
	f.Fuzz(func(t *testing.T, arg string) {
		cfg := OAuthConfig{ClientSecretsCommand: []string{"reader", arg}}
		err := cfg.Validate()
		if strings.ContainsRune(arg, 0) {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
		}
	})
}
