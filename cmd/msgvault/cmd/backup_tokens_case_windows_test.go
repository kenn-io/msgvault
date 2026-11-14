//go:build windows

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
)

func TestBackupRestoreCardDAVOnlyWithCaseVariantDirectory(t *testing.T) {
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

func TestSecureRestoredTokensSkipsConfigOnlyTarget(t *testing.T) {
	cfg, dbPath, attachments := backupTokenFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("config"), 0o600))
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	result, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir, Extras: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: configPath, RecordAs: "config.toml"}}}, IncludeConfig: true})
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "absent")
	require.NoError(t, secureRestoredTokens(t.Context(), repo, result.SnapshotID, target))
	_, err = os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
}
