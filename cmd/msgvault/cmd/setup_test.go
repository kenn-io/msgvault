package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestCreateNASBundle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")
	apiKey := "test-api-key-1234"
	port := 9090

	// Create a fake client_secret.json to copy
	secretsDir := t.TempDir()
	secretsPath := filepath.Join(secretsDir, "client_secret.json")
	secretsContent := `{"installed":{"client_id":"test"}}`
	require.NoError(os.WriteFile(secretsPath, []byte(secretsContent), 0600), "write secrets")

	err := createNASBundle(bundleDir, apiKey, secretsPath, port)
	require.NoError(err, "createNASBundle")

	// Verify config.toml exists and contains API key
	configPath := filepath.Join(bundleDir, "config.toml")
	configData, err := os.ReadFile(configPath)
	require.NoError(err, "read config.toml")
	configStr := string(configData)
	assert.Contains(configStr, apiKey, "config.toml should contain the API key")
	assert.Contains(configStr, "0.0.0.0", "config.toml should bind to 0.0.0.0")

	// Verify config.toml has secure permissions
	// Windows doesn't support Unix file permissions.
	info, err := os.Stat(configPath)
	require.NoError(err, "stat config.toml")
	if runtime.GOOS != "windows" {
		assert.Zero(info.Mode().Perm()&0077, "config.toml perm = %04o, want no group/other access", info.Mode().Perm())
	}

	// Verify client_secret.json was copied
	copiedSecrets := filepath.Join(bundleDir, "client_secret.json")
	copiedData, err := os.ReadFile(copiedSecrets)
	require.NoError(err, "read copied client_secret.json")
	assert.Equal(secretsContent, string(copiedData), "copied secrets")

	// Verify docker-compose.yml exists and contains port
	composePath := filepath.Join(bundleDir, "docker-compose.yml")
	composeData, err := os.ReadFile(composePath)
	require.NoError(err, "read docker-compose.yml")
	composeStr := string(composeData)
	assert.Contains(composeStr, "9090:8080", "docker-compose.yml should map port 9090:8080")
	assert.Contains(composeStr, "ghcr.io/kenn-io/msgvault", "docker-compose.yml should reference the msgvault image")
	assert.Contains(
		composeStr,
		"pull_policy: always",
		"docker-compose.yml should refresh the latest image during reconciliation",
	)
}

func TestCreateNASBundle_NoSecrets(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")

	err := createNASBundle(bundleDir, "key", "", 8080)
	require.NoError(
		err, "createNASBundle")

	// config.toml and docker-compose.yml should exist
	_, err = os.Stat(filepath.Join(bundleDir, "config.toml"))
	require.NoError(
		err, "config.toml should exist")

	_, err = os.Stat(filepath.Join(bundleDir, "docker-compose.yml"))
	require.NoError(
		err, "docker-compose.yml should exist")

	// client_secret.json should NOT exist (no source path given)
	_, err = os.Stat(filepath.Join(bundleDir, "client_secret.json"))
	assert.True(os.IsNotExist(err), "client_secret.json should not exist when no secrets path given")

	// config.toml must not point at a credential the bundle does not hold
	cfgData, err := os.ReadFile(filepath.Join(bundleDir, "config.toml"))
	require.NoError(err, "read config.toml")
	assert.NotContains(string(cfgData), "[oauth]")
}

// runSetupForTest runs the wizard against a fresh home directory with
// the given answers on stdin and returns the home directory and output.
func runSetupForTest(t *testing.T, answers string) (string, string) {
	t.Helper()
	home := t.TempDir()
	c := config.NewDefaultConfig()
	c.HomeDir = home
	c.Data.DataDir = home
	withTestConfig(t, c)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(answers))
	cmd.SetOut(&out)
	require.NoError(t, runSetup(cmd, nil))
	return home, out.String()
}

func TestSetupWithoutGoogleCredentials(t *testing.T) {
	// Enter at the credential prompt, then "n" for the remote server.
	home, out := runSetupForTest(t, "\nn\n")

	assert.NotContains(t, out, "add-account")
	assert.Contains(t, out, "add-imap")
	_, err := os.Stat(filepath.Join(home, "nas-bundle"))
	assert.True(t, os.IsNotExist(err), "no NAS bundle without a remote")
}

func TestSetupWithGoogleCredentialsPrintsGmailSteps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	secretsPath := filepath.Join(t.TempDir(), "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(`{"installed":{}}`), 0600))

	home, out := runSetupForTest(t, secretsPath+"\nn\n")

	assert.Contains(out, "add-account")
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(err, "setup should save config.toml")
	assert.Contains(string(data), "client_secret.json")
}

func TestCreateNASBundle_CopiesSecrets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(`{"installed":{}}`), 0600), "write secrets")

	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")
	err := createNASBundle(bundleDir, "key", secretsPath, 8080)
	require.NoError(err, "createNASBundle")

	// client_secret.json should be copied with correct content
	copied := filepath.Join(bundleDir, "client_secret.json")
	data, err := os.ReadFile(copied)
	require.NoError(err, "client_secret.json should exist")
	assert.JSONEq(`{"installed":{}}`, string(data), "copied content")

	// config.toml should reference /data/client_secret.json
	cfgData, err := os.ReadFile(filepath.Join(bundleDir, "config.toml"))
	require.NoError(err, "read config.toml")
	assert.Contains(string(cfgData), `/data/client_secret.json`, "config.toml should reference /data/client_secret.json")
}

func TestCreateNASBundle_InvalidSecretPath(t *testing.T) {
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")

	err := createNASBundle(bundleDir, "key", "/nonexistent/secret.json", 8080)
	require.Error(t, err, "createNASBundle should fail with nonexistent secrets path")
}

func TestGenerateAPIKey(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	key1, err := generateAPIKey()
	require.NoError(err, "generateAPIKey")

	// Should be 64 hex chars (32 bytes)
	assert.Len(key1, 64, "key length")

	// Should be different each time
	key2, err := generateAPIKey()
	require.NoError(err, "generateAPIKey")
	assert.NotEqual(key1, key2, "generateAPIKey should return unique keys")
}

func TestSetupAddAccountCommand(t *testing.T) {
	tests := []struct {
		name  string
		oauth config.OAuthConfig
		want  string
	}{
		{"none", config.OAuthConfig{}, ""},
		{"default secrets", config.OAuthConfig{ClientSecrets: "/c.json"}, "msgvault add-account you@gmail.com"},
		{"service account", config.OAuthConfig{ServiceAccountKey: "/sa.json"}, "msgvault add-account you@gmail.com"},
		{
			"named apps only",
			config.OAuthConfig{Apps: map[string]config.OAuthApp{
				"work":  {ClientSecrets: "/w.json"},
				"empty": {},
			}},
			"msgvault add-account you@gmail.com --oauth-app 'work'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, setupAddAccountCommand(&tt.oauth))
		})
	}
}

func TestPrintSetupNextStepsExportNeedsBundledSecrets(t *testing.T) {
	assert := assert.New(t)
	const named = "msgvault add-account you@gmail.com --oauth-app 'work'"

	var withSecrets bytes.Buffer
	printSetupNextSteps(&withSecrets, "msgvault add-account you@gmail.com", true, true)
	assert.Contains(withSecrets.String(), "msgvault export-token")

	var namedOnly bytes.Buffer
	printSetupNextSteps(&namedOnly, named, true, false)
	assert.NotContains(namedOnly.String(), "msgvault export-token")
	assert.Contains(namedOnly.String(), "cannot be exported to the NAS")
}

func TestCreateNASBundle_RebuildWithoutSecretsRemovesOldCopy(t *testing.T) {
	require := require.New(t)
	secretsPath := filepath.Join(t.TempDir(), "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(`{"installed":{}}`), 0600))
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")

	require.NoError(createNASBundle(bundleDir, "key", secretsPath, 8080))
	require.NoError(createNASBundle(bundleDir, "key", "", 8080))

	_, err := os.Stat(filepath.Join(bundleDir, "client_secret.json"))
	assert.True(t, os.IsNotExist(err), "rebuild without secrets should remove the old copy")
}

func TestPrintSetupNextStepsOmitsLocalImportForRemote(t *testing.T) {
	var local, remote bytes.Buffer
	printSetupNextSteps(&local, "", false, false)
	printSetupNextSteps(&remote, "", true, false)

	assert.Contains(t, local.String(), "import-mbox")
	assert.NotContains(t, remote.String(), "import-mbox")
}
