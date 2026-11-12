package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
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
}

func TestBackupRestoreConfigOnlyLeavesUnrelatedTokenPath(t *testing.T) {
	require := require.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(configPath, []byte("# synthetic config\n"), 0o600))
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		Extras:        backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: configPath, RecordAs: "config.toml", Sensitive: true}}},
		IncludeConfig: true, AllowPlaintextSecrets: true,
	})
	require.NoError(err)
	prepareBackupTokenRestore(t)
	target := filepath.Join(t.TempDir(), "restored")
	require.NoError(os.Mkdir(target, 0o700))
	tokenPath := filepath.Join(target, "tokens")
	require.NoError(os.WriteFile(tokenPath, []byte("unrelated"), 0o600))
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1", "--overwrite"})
	require.NoError(executeRootContext(t.Context(), rootCmd))
	data, err := os.ReadFile(tokenPath)
	require.NoError(err)
	require.Equal("unrelated", string(data))
}
