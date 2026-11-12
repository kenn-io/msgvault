package cmd

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Ctrl-C can land while the import commits its last writes. The import
// itself succeeded, so recording the owner's default identity and the
// post-import migrations must still finish instead of failing the command.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportGvoiceCancelAtFinalCommitFinishesPostImportSteps(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	testCfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, testCfg)
	markDaemonCLISubprocessForTest(t)
	voice := writeGvoiceCLIExport(t)

	queryCtx := context.WithoutCancel(t.Context())
	probe, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(probe.Close()) })
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var counted int
		require.NoError(probe.DB().QueryRowContext(queryCtx,
			`SELECT COALESCE(SUM(message_count), 0) FROM conversations`).Scan(&counted))
		return counted > 0
	})

	cmd := &cobra.Command{Use: "import-gvoice"}
	cmd.SetContext(ctx)
	runErr := runImportGvoice(cmd, []string{voice})
	require.True(canceled(), "the cancel must land after the import commits")
	require.NoError(runErr)

	var sourceID int64
	require.NoError(probe.DB().QueryRowContext(t.Context(),
		`SELECT id FROM sources WHERE source_type = 'google_voice'`).Scan(&sourceID))
	identities, err := probe.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("+15550000000", identities[0].Address)
}
