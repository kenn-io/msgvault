//go:build !linux && !darwin

package peoplesweep

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialStoreFailsClosedOnUnsupportedPlatform(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)

	err := store.Save("profile", NewCredential(AuthBearer, "unsupported-platform-test-value"))
	require.ErrorIs(err, errCredentialStoreUnsupported)
	guard, err := store.PreflightDelete("profile")
	require.ErrorIs(err, errCredentialStoreUnsupported)
	assert.Nil(guard)
	err = store.Delete("profile", nil)
	require.ErrorIs(err, errCredentialStoreUnsupported)
	assert.NoDirExists(filepath.Join(tokensDir, credentialNamespace))
}
