//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
)

func TestBackupRestoreCardDAVCredentialRemainsUsable(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		name := "fresh restore"
		if overwrite {
			name = "overwrite existing credential"
		}
		t.Run(name, func(t *testing.T) {
			cfg, dbPath, attachments := backupTokenFixture(t)
			credential := carddav.Credential{Password: "synthetic-password", BaseURL: "https://carddav.example.test", Username: "person@example.test", ConnectionGeneration: 1}
			dir := filepath.Join(cfg.TokensDir(), "carddav-connections", "work")
			require.NoError(t, carddav.SaveCredential(dir, credential))
			repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
			prepareBackupTokenRestore(t)

			target := filepath.Join(t.TempDir(), "restored")
			args := []string{"--target", target}
			if overwrite {
				upper := filepath.Join(target, "tokens", "CARDDAV-CONNECTIONS", "WORK")
				require.NoError(t, os.MkdirAll(upper, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(upper, "CARDDAV.JSON"), []byte("old"), 0o600))
				args = append(args, "--overwrite")
			}
			_, err := runBackupTokenRestore(t, cfg, repo, args...)
			require.NoError(t, err)
			got, err := carddav.LoadCredential(filepath.Join(target, "tokens", "carddav-connections", "work"))
			require.NoError(t, err)
			assert.Equal(t, credential, got)
		})
	}
}
