package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestCreateNASBundle(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	assert.Contains(composeStr, "ghcr.io/wesm/msgvault", "docker-compose.yml should reference the msgvault image")
}

func TestCreateNASBundle_NoSecrets(t *testing.T) {
	assert := assertpkg.New(t)
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")

	err := createNASBundle(bundleDir, "key", "", 8080)
	requirepkg.NoError(t, err, "createNASBundle")

	// config.toml and docker-compose.yml should exist
	_, err = os.Stat(filepath.Join(bundleDir, "config.toml"))
	requirepkg.NoError(t, err, "config.toml should exist")
	_, err = os.Stat(filepath.Join(bundleDir, "docker-compose.yml"))
	requirepkg.NoError(t, err, "docker-compose.yml should exist")

	// client_secret.json should NOT exist (no source path given)
	_, err = os.Stat(filepath.Join(bundleDir, "client_secret.json"))
	assert.True(os.IsNotExist(err), "client_secret.json should not exist when no secrets path given")
}

func TestCreateNASBundle_CopiesSecrets(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "createNASBundle should fail with nonexistent secrets path")
}

func TestGenerateAPIKey(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	key1, err := generateAPIKey()
	require.NoError(err, "generateAPIKey")

	// Should be 64 hex chars (32 bytes)
	assert.Len(key1, 64, "key length")

	// Should be different each time
	key2, err := generateAPIKey()
	require.NoError(err, "generateAPIKey")
	assert.NotEqual(key1, key2, "generateAPIKey should return unique keys")
}
