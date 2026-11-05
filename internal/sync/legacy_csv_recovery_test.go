package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestFullSyncLegacyCSVRecoveryPreservesRawAndIndexes(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	raw := testemail.LegacyCSV(true, testemail.LegacyCSVOptions{
		Body:      "<p>uniquerecoverysearchtoken</p>",
		StrayLine: " --------------040908010102080206010104",
		Header:    "Content-Description: exported contacts\r\n",
	})
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("legacy-csv-fixture", raw, []string{"INBOX"})
	attachmentDir := withAttachmentsDir(t, env)
	runFullSync(t, env)
	var id int64
	require.NoError(env.Store.DB().QueryRow("SELECT id FROM messages WHERE source_message_id=?", "legacy-csv-fixture").Scan(&id))
	storedRaw, err := env.Store.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, storedRaw)
	body, err := env.Store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Contains(body, "uniquerecoverysearchtoken")
	assert.NotContains(body, "MIME parsing failed")
	var indexed int
	var storagePath string
	require.NoError(env.Store.DB().QueryRow("SELECT storage_path FROM attachments WHERE message_id=?", id).Scan(&storagePath))
	content, err := os.ReadFile(filepath.Join(attachmentDir, storagePath))
	require.NoError(err)
	assert.Equal([]byte("first,last\r\nSample,Person"), content)
	require.NoError(env.Store.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE rowid=? AND messages_fts MATCH 'uniquerecoverysearchtoken'", id).Scan(&indexed))
	assert.Equal(1, indexed)
}
