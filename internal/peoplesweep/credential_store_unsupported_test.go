//go:build !linux && !darwin

package peoplesweep

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialStoreFailsClosedOnUnsupportedPlatform(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)

	err := store.Save("profile", NewCredential(AuthBearer, "unsupported-platform-test-value"))
	require.ErrorIs(t, err, errCredentialStoreUnsupported)
	guard, err := store.PreflightDelete("profile")
	require.ErrorIs(t, err, errCredentialStoreUnsupported)
	assert.Nil(t, guard)
	err = store.Delete("profile", nil)
	require.ErrorIs(t, err, errCredentialStoreUnsupported)
	assert.NoDirExists(t, filepath.Join(tokensDir, credentialNamespace))
}
