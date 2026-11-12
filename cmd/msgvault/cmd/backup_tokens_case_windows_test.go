//go:build windows

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func TestBackupRestoreCaseVariantProviderCredential(t *testing.T) {
	cfg, dbPath, attachments := backupTokenFixture(t)
	const endpoint = "https://provider.example.test/v1"
	const key = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(t, err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, key)
	require.NoError(t, err)
	canonical := filepath.Join(cfg.TokensDir(), providercredentials.Filename)
	temporary := filepath.Join(cfg.TokensDir(), "temporary-credentials")
	require.NoError(t, os.Rename(canonical, temporary))
	require.NoError(t, os.Rename(temporary, filepath.Join(cfg.TokensDir(), "Provider-Credentials.json")))
	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
	prepareBackupTokenRestore(t)
	target := filepath.Join(t.TempDir(), "restored")
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1"})
	require.NoError(t, executeRootContext(t.Context(), rootCmd))
	restored, err := providercredentials.Read(filepath.Join(target, "tokens"))
	require.NoError(t, err)
	value, _, err := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
	require.NoError(t, err)
	assert.Equal(t, key, value)
}
