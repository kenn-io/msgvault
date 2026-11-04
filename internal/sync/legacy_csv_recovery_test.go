package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFullSyncLegacyCSVRecoveryPreservesRawAndIndexes(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	// The provider is the existing Gmail test double; parsing, attachment
	// publication, store transactions, compression, statistics and FTS are real.
	raw := []byte("From: sender@example.org\r\nTo: recipient@example.org\r\n" +
		"Subject: synthetic legacy recovery\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=related\r\n\r\n" +
		"--related\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>uniquerecoverysearchtoken</p>\r\n" +
		"--related\r\n --------------040908010102080206010104\r\n" +
		"Content-Type: text/csv; name=example.csv\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment; filename=example.csv\r\n" +
		"Content-Description: exported contacts\r\n\r\nfirst,last\r\nSample,Person\r\n" +
		"--related--\r\n")
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("legacy-csv-fixture", raw, []string{"INBOX"})
	attachmentDir := withAttachmentsDir(t, env)
	summary := runFullSync(t, env)
	assertSummary(t, summary, WantSummary{Added: new(int64(1)), Errors: new(int64(0))})
	var id int64
	require.NoError(env.Store.DB().QueryRow("SELECT id FROM messages WHERE source_message_id=?", "legacy-csv-fixture").Scan(&id))
	storedRaw, err := env.Store.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, storedRaw)
	body, err := env.Store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Contains(body, "uniquerecoverysearchtoken")
	assert.NotContains(body, "MIME parsing failed")
	var count, indexed int
	var declaredCount int
	var hasAttachments bool
	var contentHash, storagePath string
	require.NoError(env.Store.DB().QueryRow("SELECT count(*) FROM attachments WHERE message_id=?", id).Scan(&count))
	assert.Equal(1, count)
	require.NoError(env.Store.DB().QueryRow("SELECT attachment_count,has_attachments FROM messages WHERE id=?", id).Scan(&declaredCount, &hasAttachments))
	assert.Equal(1, declaredCount)
	assert.True(hasAttachments)
	require.NoError(env.Store.DB().QueryRow("SELECT content_hash,storage_path FROM attachments WHERE message_id=?", id).Scan(&contentHash, &storagePath))
	content, err := os.ReadFile(filepath.Join(attachmentDir, storagePath))
	require.NoError(err)
	digest := sha256.Sum256(content)
	assert.Equal(contentHash, hex.EncodeToString(digest[:]))
	assert.Equal([]byte("first,last\r\nSample,Person"), content)
	require.NoError(env.Store.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE rowid=? AND messages_fts MATCH 'uniquerecoverysearchtoken'", id).Scan(&indexed))
	assert.Equal(1, indexed)
	assert.Equal(1, countFiles(t, attachmentDir))
}
