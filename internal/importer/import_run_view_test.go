package importer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The native directory dispatcher must carry its validated logical run into
// canonical persistence, rather than merely setting a silent ingestion mode.
func TestImportEMLDirRejectsRevokedRunBeforeRelatedWrites(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st, tmp := testutil.NewTestStore(t), t.TempDir()
	const owner = "owner:synthetic-directory-import"
	cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true}
	_, err := st.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	root := filepath.Join(tmp, "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <native-view@example.net>\r\nSubject: Synthetic import view\r\n\r\nSynthetic body\r\n")
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), raw, 0o600))
	ingest := rawMessageIngester(nil, true)
	opts := EMLImportOptions{Identifier: "owner@example.net", NoResume: true}
	opts.IngestFunc = func(ctx context.Context, view *store.Store, sourceID int64, identifier, attachmentsDir string, labelIDs []int64, sourceMsgID, rawHash string, raw []byte, fallbackDate time.Time, log *slog.Logger) error {
		// Simulate an owner change between native parser dispatch and its SQL
		// snapshot. The parser and canonical ingester remain the production path.
		cfg.Principal = "owner:synthetic-directory-replacement"
		if _, err := st.ConfigureMCPEvents(ctx, cfg); err != nil {
			return err
		}
		return ingest(ctx, view, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log)
	}
	summary, err := ImportEMLDir(t.Context(), st, root, opts)
	require.NoError(err, "the native directory API reports item failures in its summary")
	require.NotNil(summary)
	assert.True(summary.HardErrors, "owner revocation rejects the native canonical message write")
	assert.Equal(int64(1), summary.Errors)
	assert.Zero(summary.MessagesAdded)
	var messages int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Zero(messages, "no canonical message may commit under the replaced run owner")
	var participants int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participants`).Scan(&participants))
	assert.Zero(participants, "rejected run authority must not leave related identity writes")
}
