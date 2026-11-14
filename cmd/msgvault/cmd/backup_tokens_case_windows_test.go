//go:build windows

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/providercredentials"
	"golang.org/x/sys/windows"
)

func TestBackupRestoreCredentialsRemainUsable(t *testing.T) {
	t.Run("CardDAV only with case variant", func(t *testing.T) {
		cfg, dbPath, attachments := backupTokenFixture(t)
		credential := carddav.Credential{Password: "synthetic-password", BaseURL: "https://carddav.example.test", Username: "person@example.test", ConnectionGeneration: 1}
		dir := filepath.Join(cfg.TokensDir(), "carddav-connections", "work")
		require.NoError(t, carddav.SaveCredential(dir, credential))
		repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
		prepareBackupTokenRestore(t)
		target := filepath.Join(t.TempDir(), "restored")
		upper := filepath.Join(target, "tokens", "CARDDAV-CONNECTIONS", "WORK")
		require.NoError(t, os.MkdirAll(upper, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(upper, "CARDDAV.JSON"), []byte("old"), 0o600))
		_, err := runBackupTokenRestore(t, cfg, repo, "--target", target, "--overwrite")
		require.NoError(t, err)
		got, err := carddav.LoadCredential(filepath.Join(target, "tokens", "carddav-connections", "work"))
		require.NoError(t, err)
		assert.Equal(t, credential, got)
	})
	t.Run("provider and server key", func(t *testing.T) {
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

		fresh := filepath.Join(t.TempDir(), "fresh")
		_, err = runBackupTokenRestore(t, cfg, repo, "--target", fresh)
		require.NoError(err)
		freshTokens := filepath.Join(fresh, "tokens")
		restored, err := providercredentials.Read(freshTokens)
		require.NoError(err)
		value, _, err := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
		require.NoError(err)
		assert.Equal(providerKey, value)
		key, err := providercredentials.ReadSecretFile(filepath.Join(freshTokens, providercredentials.ServerKeyFilename))
		require.NoError(err)
		assert.Equal(serverKey, key)
	})
}

func TestBackupRestoreConfigOnlySkipsTokensAndHonorsForceUnlock(t *testing.T) {
	cfg, dbPath, attachments := backupTokenFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("# config"), 0o600))
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		Extras: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: configPath, RecordAs: "config.toml"}}}, IncludeConfig: true,
	})
	require.NoError(t, err)
	target := t.TempDir()
	tokens := filepath.Join(target, "tokens")
	require.NoError(t, os.Mkdir(tokens, 0o700))
	deny, err := windows.SecurityDescriptorFromString("D:P")
	require.NoError(t, err)
	empty, _, err := deny.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, empty, nil))
	t.Cleanup(func() {
		// The owner keeps WRITE_DAC; a null DACL lets TempDir cleanup remove the directory.
		assert.NoError(t, windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil))
	})
	locked, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	_, err = os.ReadDir(tokens)
	require.Error(t, err)
	prepareBackupTokenRestore(t)
	lock := repo.Path("locks", "exclusive.json")
	lockInfo, err := json.Marshal(backup.LockInfo{Hostname: "backup.example.test", PID: 1234, Operation: "create", AcquiredAt: time.Now().UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lock, lockInfo, 0o600))
	args := []string{manifest.SnapshotID, "--target", target, "--permissions-only"}
	_, err = runBackupTokenRestore(t, cfg, repo, args...)
	require.ErrorIs(t, err, backup.ErrRepoLocked)
	output, err := runBackupTokenRestore(t, cfg, repo, append(args, "--force-unlock")...)
	require.NoError(t, err)
	assert.Contains(t, output, "secured token files: 0")
	after, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	assert.Equal(t, locked.String(), after.String())
}
