package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Ctrl-C during a first Telegram import leaves a new source with committed
// messages. The post-import step that records the owner's default identity
// must still run, or those messages stay without an owner.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportTelegramInterruptedFirstImportRecordsDefaultIdentity(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "result.json"), []byte(`{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","from_id":"user1","text":"first"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","from_id":"user1","text":"second"}]}]}}`), 0600))

	queryCtx := context.WithoutCancel(t.Context())
	probe, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(probe.Close()) })
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(probe.DB().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
		return count > 0
	})

	cmd := &cobra.Command{Use: "import-telegram"}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.ErrorIs(runImportTelegram(cmd, root, telegramCLIOptions{}), context.Canceled)
	require.True(canceled(), "the cancel must land after the first message commits")

	var sourceID int64
	require.NoError(probe.DB().QueryRowContext(t.Context(),
		`SELECT id FROM sources WHERE source_type = 'telegram'`).Scan(&sourceID))
	identities, err := probe.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	assert.NotEmpty(identities, "an interrupted first import must record the default identity")
}
