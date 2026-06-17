package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeServiceAccountKey(t *testing.T, path string, perm os.FileMode) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err, "GenerateKey")
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err, "MarshalPKCS8PrivateKey")
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})

	data, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "test-key-id",
		"private_key":    string(pemKey),
		"client_email":   "svc@test-project.iam.gserviceaccount.com",
		"client_id":      "123456789",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	require.NoError(t, err, "Marshal")
	require.NoError(t, os.WriteFile(path, data, perm), "WriteFile")
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(path, perm), "Chmod")
	}
}

func TestNewServiceAccountManagerRejectsInsecureKeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}

	path := filepath.Join(t.TempDir(), "service-account.json")
	writeServiceAccountKey(t, path, 0644)

	_, err := NewServiceAccountManager(path, Scopes)
	require.Error(t, err, "expected insecure permission error")
	assert.ErrorContains(t, err, "service account key permissions")
}

func TestNewServiceAccountManagerAcceptsOwnerOnlyKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-account.json")
	writeServiceAccountKey(t, path, 0600)

	_, err := NewServiceAccountManager(path, Scopes)
	require.NoError(t, err, "NewServiceAccountManager")
}

func TestNewServiceAccountManagerRejectsEmptyScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-account.json")
	writeServiceAccountKey(t, path, 0600)

	_, err := NewServiceAccountManager(path, nil)
	require.Error(t, err, "expected empty scopes error")
	assert.ErrorContains(t, err, "at least one scope")
}

func TestNewServiceAccountManagerRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-account.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0600), "WriteFile")

	_, err := NewServiceAccountManager(path, Scopes)
	require.Error(t, err, "expected parse error")
	assert.ErrorContains(t, err, "parse service account key")
}
