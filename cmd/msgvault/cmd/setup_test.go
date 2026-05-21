package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreateNASBundle(t *testing.T) {
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")
	apiKey := "test-api-key-1234"
	port := 9090

	// Create a fake client_secret.json to copy
	secretsDir := t.TempDir()
	secretsPath := filepath.Join(secretsDir, "client_secret.json")
	secretsContent := `{"installed":{"client_id":"test"}}`
	if err := os.WriteFile(secretsPath, []byte(secretsContent), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	err := createNASBundle(bundleDir, apiKey, secretsPath, port)
	if err != nil {
		t.Fatalf("createNASBundle error = %v", err)
	}

	// Verify config.toml exists and contains API key
	configPath := filepath.Join(bundleDir, "config.toml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	configStr := string(configData)
	if !strings.Contains(configStr, apiKey) {
		t.Error("config.toml should contain the API key")
	}
	if !strings.Contains(configStr, "0.0.0.0") {
		t.Error("config.toml should bind to 0.0.0.0")
	}
	if !strings.Contains(configStr, "[oauth]") {
		t.Error("config.toml should contain [oauth] section when secrets are provided")
	}
	if !strings.Contains(configStr, `client_secrets = "/data/client_secret.json"`) {
		t.Error("config.toml should set client_secrets to /data/client_secret.json")
	}

	// Verify config.toml has secure permissions
	// Windows doesn't support Unix file permissions.
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config.toml: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Errorf("config.toml perm = %04o, want no group/other access", info.Mode().Perm())
	}

	// Verify client_secret.json was copied
	copiedSecrets := filepath.Join(bundleDir, "client_secret.json")
	copiedData, err := os.ReadFile(copiedSecrets)
	if err != nil {
		t.Fatalf("read copied client_secret.json: %v", err)
	}
	if string(copiedData) != secretsContent {
		t.Errorf("copied secrets = %q, want %q", string(copiedData), secretsContent)
	}

	// Verify docker-compose.yml exists and contains port
	composePath := filepath.Join(bundleDir, "docker-compose.yml")
	composeData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	composeStr := string(composeData)
	if !strings.Contains(composeStr, "9090:8080") {
		t.Error("docker-compose.yml should map port 9090:8080")
	}
	if !strings.Contains(composeStr, "ghcr.io/wesm/msgvault") {
		t.Error("docker-compose.yml should reference the msgvault image")
	}
}

func TestCreateNASBundle_NoSecrets(t *testing.T) {
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")

	err := createNASBundle(bundleDir, "key", "", 8080)
	if err != nil {
		t.Fatalf("createNASBundle error = %v", err)
	}

	// config.toml and docker-compose.yml should exist
	if _, err := os.Stat(filepath.Join(bundleDir, "config.toml")); err != nil {
		t.Error("config.toml should exist")
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "docker-compose.yml")); err != nil {
		t.Error("docker-compose.yml should exist")
	}

	// client_secret.json should NOT exist (no source path given)
	if _, err := os.Stat(filepath.Join(bundleDir, "client_secret.json")); !os.IsNotExist(err) {
		t.Error("client_secret.json should not exist when no secrets path given")
	}

	// config.toml should NOT contain [oauth] section when no secrets provided;
	// the NAS instance falls back to the embedded verified OAuth client.
	configBytes, err := os.ReadFile(filepath.Join(bundleDir, "config.toml"))
	if err != nil {
		t.Fatalf("read bundle config: %v", err)
	}
	if strings.Contains(string(configBytes), "[oauth]") {
		t.Error("config.toml should not contain [oauth] section when no secrets provided")
	}
}

func TestCreateNASBundle_CopiesSecrets(t *testing.T) {
	tmpDir := t.TempDir()
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	if err := os.WriteFile(secretsPath, []byte(`{"installed":{}}`), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")
	err := createNASBundle(bundleDir, "key", secretsPath, 8080)
	if err != nil {
		t.Fatalf("createNASBundle error = %v", err)
	}

	// client_secret.json should be copied with correct content
	copied := filepath.Join(bundleDir, "client_secret.json")
	data, err := os.ReadFile(copied)
	if err != nil {
		t.Fatalf("client_secret.json should exist: %v", err)
	}
	if string(data) != `{"installed":{}}` {
		t.Errorf("copied content = %q, want original", string(data))
	}

	// config.toml should reference /data/client_secret.json
	cfgData, err := os.ReadFile(filepath.Join(bundleDir, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	cfgStr := string(cfgData)
	if !strings.Contains(cfgStr, "[oauth]") {
		t.Error("config.toml should contain [oauth] section when secrets are provided")
	}
	if !strings.Contains(cfgStr, `/data/client_secret.json`) {
		t.Error("config.toml should reference /data/client_secret.json")
	}
}

func TestCreateNASBundle_InvalidSecretPath(t *testing.T) {
	bundleDir := filepath.Join(t.TempDir(), "nas-bundle")

	err := createNASBundle(bundleDir, "key", "/nonexistent/secret.json", 8080)
	if err == nil {
		t.Fatal("createNASBundle should fail with nonexistent secrets path")
	}
}

func TestGenerateAPIKey(t *testing.T) {
	key1, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey error = %v", err)
	}

	// Should be 64 hex chars (32 bytes)
	if len(key1) != 64 {
		t.Errorf("key length = %d, want 64", len(key1))
	}

	// Should be different each time
	key2, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generateAPIKey error = %v", err)
	}
	if key1 == key2 {
		t.Error("generateAPIKey should return unique keys")
	}
}
