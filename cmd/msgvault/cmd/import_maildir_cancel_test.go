package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Ctrl-C can land while the importer commits the finished sync. The import
// itself succeeded, so the default identity must still be recorded before the
// legacy identity migration runs.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportMaildirCancelDuringFinalSyncCommitRecordsDefaultIdentity(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	testCtx := withStoreResolverConfig(t, testCfg)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	root := filepath.Join(dataDir, "mail")
	for _, dir := range []string{"new", "cur", "tmp"} {
		require.NoError(os.MkdirAll(filepath.Join(root, dir), 0700))
	}
	require.NoError(os.WriteFile(filepath.Join(root, "new", "one"), []byte("From: alice@example.com\r\nSubject: Maildir CLI\r\n\r\nbody"), 0600))

	queryCtx := context.WithoutCancel(t.Context())
	probe, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(probe.Close()) })
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(probe.DB().QueryRowContext(queryCtx,
			`SELECT COUNT(*) FROM sync_runs WHERE status = 'completed'`).Scan(&count))
		return count > 0
	})

	command := newImportMaildirCommand()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{root, "--identifier", "alice@example.com"})
	require.ErrorIs(command.Execute(), context.Canceled)
	require.True(canceled(), "the cancel must land after the sync completes")

	var sourceID int64
	require.NoError(probe.DB().QueryRowContext(t.Context(),
		`SELECT id FROM sources WHERE source_type = 'maildir'`).Scan(&sourceID))
	identities, err := probe.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	require.NotEmpty(identities, "a cancel after the final commit must still record the default identity")
	assert.Equal("alice@example.com", identities[0].Address)
}
