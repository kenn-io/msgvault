package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func TestBackupRestoreNestedTokensRemainUsable(t *testing.T) {
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

	nested := filepath.Join(cfg.TokensDir(), "carddav-connections", "example")
	credential := carddav.Credential{Password: "synthetic-password", BaseURL: "https://carddav.example.test", Username: "person@example.test", ConnectionGeneration: 1}
	require.NoError(carddav.SaveCredential(nested, credential))

	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)

	prepareBackupTokenRestore(t)

	fresh := filepath.Join(t.TempDir(), "fresh")
	require.NoError(runBackupTokenRestore(t, cfg, repo, fresh, false))
	freshTokens := filepath.Join(fresh, "tokens")
	restored, err := providercredentials.Read(freshTokens)
	require.NoError(err)
	value, _, err := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
	require.NoError(err)
	assert.Equal(providerKey, value)
	key, err := providercredentials.ReadSecretFile(filepath.Join(freshTokens, providercredentials.ServerKeyFilename))
	require.NoError(err)
	assert.Equal(serverKey, key)
	freshNested, err := carddav.LoadCredential(filepath.Join(freshTokens, "carddav-connections", "example"))
	require.NoError(err)
	assert.Equal(credential, freshNested)
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
	require.NoError(runBackupTokenRestore(t, cfg, repo, target, true))
	data, err := os.ReadFile(tokenPath)
	require.NoError(err)
	require.Equal("unrelated", string(data))
}

func TestBackupRestorePermissionsOnlyRequiresSnapshot(t *testing.T) {
	cfg, dbPath, attachments := backupTokenFixture(t)
	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
	prepareBackupTokenRestore(t)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", filepath.Join(t.TempDir(), "target"), "--permissions-only"})
	err := executeRootContext(t.Context(), rootCmd)
	require.ErrorContains(t, err, "requires an explicit snapshot ID")
}
