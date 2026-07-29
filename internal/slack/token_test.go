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
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	require.NoError(SaveToken(dir, "T01", "testers", "UME", "xoxp-me"))
	require.NoError(SaveToken(dir, "T01", "testers", "UOTHER", "xoxp-other"))

	mine, err := LoadToken(dir, "T01", "UME")
	require.NoError(err)
	other, err := LoadToken(dir, "T01", "UOTHER")
	require.NoError(err)
	assert.Equal("xoxp-me", mine)
	assert.Equal("xoxp-other", other)

	require.NoError(DeleteToken(dir, "T01", "UOTHER"))
	mine, err = LoadToken(dir, "T01", "UME")
	require.NoError(err, "removing one user's token must not touch the other's")
	assert.Equal("xoxp-me", mine)
	_, err = LoadToken(dir, "T01", "UOTHER")
	require.ErrorContains(err, "run 'add-slack' first")
}

func TestLoadTokenRejectsIdentityMismatch(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(SaveToken(dir, "T01", "testers", "UME", "xoxp-me"))

	// A token file renamed or copied across identities must not be served.
	src := tokenPath(dir, "T01", "UME")
	dst := tokenPath(dir, "T01", "UIMPOSTOR")
	data, err := os.ReadFile(src)
	require.NoError(err)
	require.NoError(os.WriteFile(dst, data, 0o600))

	_, err = LoadToken(dir, "T01", "UIMPOSTOR")
	require.ErrorContains(err, "holds identity")
}
