//go:build windows

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/providercredentials"
	"golang.org/x/sys/windows"
)

func TestBackupRestoreCardDAVOnly(t *testing.T) {
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
	require.NoError(t, runBackupTokenRestore(t, cfg, repo, target, true))
	got, err := carddav.LoadCredential(filepath.Join(target, "tokens", "carddav-connections", "work"))
	require.NoError(t, err)
	assert.Equal(t, credential, got)
}

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
}

func TestBackupRestoreConfigOnlyLeavesUnrelatedTokenPath(t *testing.T) {
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
	require.NoError(t, os.WriteFile(filepath.Join(tokens, "omitted"), []byte("unchanged"), 0o600))
	path, err := windows.UTF16PtrFromString(tokens)
	require.NoError(t, err)
	handle, err := windows.CreateFile(path, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(t, err)
	before, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := before.Control()
	require.NoError(t, err)
	flags := windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	t.Cleanup(func() {
		assert.NoError(t, windows.SetKernelObjectSecurity(handle, windows.SECURITY_INFORMATION(flags), before))
		assert.NoError(t, windows.CloseHandle(handle))
	})
	deny, err := windows.SecurityDescriptorFromString("D:P")
	require.NoError(t, err)
	require.NoError(t, windows.SetKernelObjectSecurity(handle, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, deny))
	locked, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	_, err = os.ReadDir(tokens)
	require.Error(t, err)
	prepareBackupTokenRestore(t)
	lock := repo.Path("locks", "exclusive.json")
	require.NoError(t, os.WriteFile(lock, []byte("{}"), 0o600))
	args := []string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", manifest.SnapshotID, "--repo", repo.Root(), "--target", target, "--permissions-only"}
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetArgs(args)
	require.ErrorIs(t, executeRootContext(t.Context(), rootCmd), backup.ErrRepoLocked)
	rootCmd.SetArgs(append(args, "--force-unlock"))
	require.NoError(t, executeRootContext(t.Context(), rootCmd))
	assert.Contains(t, output.String(), "secured token files: 0 (snapshot contains no token files)")
	after, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	assert.Equal(t, locked.String(), after.String())
}
