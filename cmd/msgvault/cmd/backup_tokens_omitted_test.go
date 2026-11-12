package cmd

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func TestBackupRestoreOmittedTokensRemainReadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(cfg.TokensDir(), ".provider-credentials.lock")))

	nested := filepath.Join(cfg.TokensDir(), "carddav-connections", "example", "carddav.json")
	require.NoError(os.MkdirAll(filepath.Dir(nested), 0o700))
	require.NoError(os.WriteFile(nested, []byte("synthetic-carddav"), 0o600))

	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)

	prepareBackupTokenRestore(t)

	fresh := filepath.Join(t.TempDir(), "fresh")
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", fresh, "--jobs", "1"})
	require.NoError(executeRootContext(t.Context(), rootCmd))
	freshTokens := filepath.Join(fresh, "tokens")
	_, err = providercredentials.Read(freshTokens)
	require.NoError(err)
	freshNested, err := os.ReadFile(filepath.Join(freshTokens, "carddav-connections", "example", "carddav.json"))
	require.NoError(err)
	assert.Equal("synthetic-carddav", string(freshNested))
	for _, dir := range []string{filepath.Join(freshTokens, "carddav-connections"), filepath.Join(freshTokens, "carddav-connections", "example")} {
		_, err := os.ReadDir(dir)
		require.NoError(err)
		require.NoError(os.Mkdir(filepath.Join(dir, "new-directory"), 0o700))
	}

	target := filepath.Join(t.TempDir(), "restored")
	tokens := filepath.Join(target, "tokens")
	omittedKey, err := providercredentials.EnsureServerKey(tokens)
	require.NoError(err)
	lockPath := filepath.Join(tokens, ".provider-credentials.lock")
	if runtime.GOOS != "windows" {
		require.NoError(os.Chmod(lockPath, 0o644))
	}
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1", "--overwrite"})
	require.NoError(executeRootContext(t.Context(), rootCmd))

	restored, err := providercredentials.Read(tokens)
	require.NoError(err)
	value, _, err := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
	require.NoError(err)
	assert.Equal(providerKey, value)
	key, err := providercredentials.ReadSecretFile(filepath.Join(tokens, providercredentials.ServerKeyFilename))
	require.NoError(err, "omitted server key remains readable")
	assert.Equal(omittedKey, key)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(lockPath)
		require.NoError(err)
		assert.Equal(os.FileMode(0o644), info.Mode().Perm())
	}
	nestedData, err := os.ReadFile(filepath.Join(tokens, "carddav-connections", "example", "carddav.json"))
	require.NoError(err)
	assert.Equal("synthetic-carddav", string(nestedData))
}
