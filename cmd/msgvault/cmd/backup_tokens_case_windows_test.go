//go:build windows

package cmd

import (
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
	require.NoError(t, runBackupTokenRestore(t, cfg, repo, target, false))
	restored, err := providercredentials.Read(filepath.Join(target, "tokens"))
	require.NoError(t, err)
	value, _, err := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
	require.NoError(t, err)
	assert.Equal(t, key, value)
}

func TestBackupRestoreCaseVariantEventsCredential(t *testing.T) {
	cfg, dbPath, attachments := backupTokenFixture(t)
	id := providercredentials.EventsID("0123456789abcdef0123456789abcdef")
	empty, err := providercredentials.ReadEvents(cfg.TokensDir())
	require.NoError(t, err)
	revision, err := empty.Revision(id)
	require.NoError(t, err)
	_, err = providercredentials.PutIfRevision(cfg.TokensDir(), revision, id, "https://example.test", "synthetic-secret")
	require.NoError(t, err)
	canonical := filepath.Join(cfg.TokensDir(), providercredentials.EventsFilename)
	temporary := filepath.Join(cfg.TokensDir(), "temporary-events-credentials")
	require.NoError(t, os.Rename(canonical, temporary))
	require.NoError(t, os.Rename(temporary, filepath.Join(cfg.TokensDir(), "Mcp-Events-Credentials.json")))
	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
	prepareBackupTokenRestore(t)
	target := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, runBackupTokenRestore(t, cfg, repo, target, false))
	restored, err := providercredentials.ReadEvents(filepath.Join(target, "tokens"))
	require.NoError(t, err)
	value, _, err := restored.Resolve(id, "https://example.test", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "synthetic-secret", value)
}
