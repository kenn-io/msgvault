package peoplesweep_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPeopleCredentialRevisionCompareAndSave(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored people provider credentials are unsupported on this platform")
	}
	tokensDir := filepath.Join(t.TempDir(), "tokens")
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	initial, configured, err := store.Revision("remote")
	requireChecks.NoError(err)
	assertChecks.False(configured)
	assertChecks.NotEmpty(initial)
	first, err := store.SaveIfRevision("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "first-secret"), initial)
	requireChecks.NoError(err)
	assertChecks.NotEqual(initial, first)
	current, configured, err := peoplesweep.NewFileCredentialStore(tokensDir).Revision("remote")
	requireChecks.NoError(err)
	assertChecks.True(configured)
	assertChecks.Equal(first, current)
	assertChecks.NotContains(current, "first-secret")
	_, err = store.SaveIfRevision("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "stale-write"), initial)
	requireChecks.ErrorIs(err, peoplesweep.ErrCredentialRevisionConflict)
	loaded, err := store.Load("remote")
	requireChecks.NoError(err)
	assertChecks.Equal("first-secret", loaded.Value())
	second, err := store.SaveIfRevision("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "second-secret"), first)
	requireChecks.NoError(err)
	assertChecks.NotEqual(first, second)
	_, err = store.DeleteIfRevision("remote", first)
	requireChecks.ErrorIs(err, peoplesweep.ErrCredentialRevisionConflict)
	missing, err := store.DeleteIfRevision("remote", second)
	requireChecks.NoError(err)
	current, configured, err = store.Revision("remote")
	requireChecks.NoError(err)
	assertChecks.False(configured)
	assertChecks.Equal(missing, current)
	_, err = store.Load("remote")
	requireChecks.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}
