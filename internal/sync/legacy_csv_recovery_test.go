package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestFullSyncLegacyCSVRecovery(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	raw := testemail.LegacyCSV(true, testemail.LegacyCSVOptions{})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("legacy-csv-fixture", raw, []string{"INBOX"})
	attachmentDir := withAttachmentsDir(t, env)
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow("SELECT id FROM messages WHERE source_message_id=?", "legacy-csv-fixture").Scan(&id))
	body, err := env.Store.GetMessageBodyText(id)
	require.NoError(err)
	assert.NotContains(body, "MIME parsing failed")
	var storagePath string
	require.NoError(env.Store.DB().QueryRow("SELECT storage_path FROM attachments WHERE message_id=?", id).Scan(&storagePath))
	assert.FileExists(filepath.Join(attachmentDir, storagePath))
}
