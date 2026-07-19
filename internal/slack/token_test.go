package slack

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two users of the same workspace must hold independent tokens: a shared
// per-team file would be clobbered by the second add-slack, syncing one
// user's source with the other's token and deleting the survivor's token on
// remove-account.
func TestTokensAreIsolatedPerWorkspaceUser(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveToken(dir, "T01", "testers", "UME", "xoxp-me"))
	require.NoError(t, SaveToken(dir, "T01", "testers", "UOTHER", "xoxp-other"))

	mine, err := LoadToken(dir, "T01", "UME")
	require.NoError(t, err)
	other, err := LoadToken(dir, "T01", "UOTHER")
	require.NoError(t, err)
	assert.Equal(t, "xoxp-me", mine)
	assert.Equal(t, "xoxp-other", other)

	require.NoError(t, DeleteToken(dir, "T01", "UOTHER"))
	mine, err = LoadToken(dir, "T01", "UME")
	require.NoError(t, err, "removing one user's token must not touch the other's")
	assert.Equal(t, "xoxp-me", mine)
	_, err = LoadToken(dir, "T01", "UOTHER")
	require.ErrorContains(t, err, "run 'add-slack' first")
}

func TestLoadTokenRejectsIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveToken(dir, "T01", "testers", "UME", "xoxp-me"))

	// A token file renamed or copied across identities must not be served.
	src := tokenPath(dir, "T01", "UME")
	dst := tokenPath(dir, "T01", "UIMPOSTOR")
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, data, 0o600))

	_, err = LoadToken(dir, "T01", "UIMPOSTOR")
	require.ErrorContains(t, err, "holds identity")
}
