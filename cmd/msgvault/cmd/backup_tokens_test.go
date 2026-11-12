package cmd

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func TestBackupRestoreIncludedTokensRemainReadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	serverKey, err := providercredentials.EnsureServerKey(cfg.TokensDir())
	require.NoError(err)

	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)

	prepareBackupTokenRestore(t)

	target := filepath.Join(t.TempDir(), "restored")
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1"})
	require.NoError(executeRootContext(t.Context(), rootCmd))

	tokens := filepath.Join(target, "tokens")
	restored, err := providercredentials.Read(tokens)
	if assert.NoError(err, "restored provider credentials remain readable") {
		value, _, resolveErr := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
		if assert.NoError(resolveErr) {
			assert.Equal(providerKey, value)
		}
	}
	key, err := providercredentials.ReadSecretFile(filepath.Join(tokens, providercredentials.ServerKeyFilename))
	if assert.NoError(err, "restored server key remains readable") {
		assert.Equal(serverKey, key)
	}
}
