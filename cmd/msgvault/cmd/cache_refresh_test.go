package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

func TestRebuildCacheAfterWriteReturnsError(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	require.NoError(st.Close())

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("cache export sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return sentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	err = rebuildCacheAfterWrite(dbPath)
	require.ErrorIs(err, sentinel)
	require.ErrorContains(err, "refresh analytics cache")
}

func TestRepairEncodingReturnsCacheRefreshError(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("repair cache sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return sentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	err := runRepairEncodingLocal(&cobra.Command{})
	require.ErrorIs(err, sentinel)
	require.ErrorContains(err, "encoding repair completed")
	require.ErrorContains(err, "analytics cache refresh failed")
}

func TestScheduledCacheRefreshFailurePreservesCompletedSyncRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	const identifier = "imaps://user@example.com@imap.example.com:993"
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: 7,
		MessagesAdded:     5,
		MessagesUpdated:   2,
	}))
	require.NoError(st.CompleteSync(syncID, "cursor-2"))

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("scheduled cache sentinel")
	oldRunBuild := runBuildCacheSubprocess
	runBuildCacheSubprocess = func(context.Context, bool, bool) error { return sentinel }
	t.Cleanup(func() { runBuildCacheSubprocess = oldRunBuild })

	getOAuthMgr := func(string) (*oauth.Manager, error) {
		return nil, errors.New("unexpected Gmail OAuth path")
	}
	err = runScheduledSync(context.Background(), identifier, st, getOAuthMgr)
	require.ErrorIs(err, sentinel, "cache failure must reach the scheduled job result")
	require.ErrorContains(err, "refresh analytics cache")

	latest, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(syncID, latest.ID)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(7), latest.MessagesProcessed)
	assert.Equal(int64(5), latest.MessagesAdded)
	assert.Equal(int64(2), latest.MessagesUpdated)
}
