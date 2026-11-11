package cmd

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBackupRestoreIncludedTokensRemainReadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = dataDir
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	require.NoError(st.Close())
	attachments := filepath.Join(dataDir, "attachments")
	require.NoError(os.MkdirAll(attachments, 0o700))

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	serverKey, err := providercredentials.EnsureServerKey(cfg.TokensDir())
	require.NoError(err)

	oldInclude, oldConfig, oldPlain := backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets
	backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets = true, false, true
	t.Cleanup(func() {
		backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets = oldInclude, oldConfig, oldPlain
	})
	extras, err := backupExtrasSpec(cfg)
	require.NoError(err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: dataDir,
		Extras: extras, IncludeTokens: true, AllowPlaintextSecrets: true,
	})
	require.NoError(err)

	oldRepo, oldTarget, oldOverwrite := backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite
	oldForce, oldJobs := backupRestoreForceUnlock, backupRestoreJobs
	oldLoose, oldIntegrity := backupRestoreLooseAttachments, backupRestoreIntegrityCheck
	oldOut, oldErr, oldLogger := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr(), slog.Default()
	changed := make(map[string]bool)
	for _, name := range []string{"repo", "target", "jobs"} {
		changed[name] = backupRestoreCmd.Flags().Lookup(name).Changed
	}
	t.Cleanup(func() {
		backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite = oldRepo, oldTarget, oldOverwrite
		backupRestoreForceUnlock, backupRestoreJobs = oldForce, oldJobs
		backupRestoreLooseAttachments, backupRestoreIntegrityCheck = oldLoose, oldIntegrity
		for name, wasChanged := range changed {
			backupRestoreCmd.Flags().Lookup(name).Changed = wasChanged
		}
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		slog.SetDefault(oldLogger)
	})
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
