package whatsapp

import (
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextPersistedKeys(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	_, err := importTextForTest(t.Context(), NewImporter(st, nil), textFile(t, "13/6/26, 10:00 - Mira: asters\n13/6/26, 10:01 - You: fictional garden note\n13/6/26, 10:02 - You changed the group description: garden\n"), textOptions())
	require.NoError(err)
	// Fixed SHA-256 values independently calculated from byte-length-framed fields.
	var chat, message, senderKey string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_conversation_id FROM conversations").Scan(&chat))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_message_id FROM messages WHERE snippet = 'asters'").Scan(&message))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT identifier_value FROM participant_identifiers WHERE identifier_type = 'whatsapp_txt' AND participant_id = (SELECT sender_id FROM messages WHERE snippet = 'asters')").Scan(&senderKey))
	assert.Equal("whatsapp_txt:32c97f9e3c01ae63b595143bfee66978336626bbd70e0771d2f0d17082d5fcee", chat)
	assert.Equal("whatsapp_txt:c1d5a41dfc788949ed4e97d80a573a539d90e2a93fcc9251a6a3d14725f734fb:0", message)
	assert.Equal("f306fa6dc96fb4ed9e48408d7039d37d5d15dec20d5c8f6e324c3cfd818f8e28", senderKey)
	var messageID int64
	var metadata sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id,metadata FROM messages WHERE snippet = 'asters'").Scan(&messageID, &metadata))
	assert.False(metadata.Valid, "full provenance belongs only in message_raw")
	var rawFormat string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT raw_format FROM message_raw WHERE message_id = ?"), messageID).Scan(&rawFormat))
	assert.Equal("whatsapp_txt", rawFormat)
	raw, err := st.GetMessageRawContext(t.Context(), messageID)
	require.NoError(err)
	var provenance textProvenance
	require.NoError(json.Unmarshal(raw, &provenance))
	assert.Equal("unknown", provenance.Direction)
	assert.Equal("earlier", provenance.AmbiguousTimePolicy)
	assert.Equal("asters", provenance.Body)
	assert.Equal("13/6/26, 10:00 - Mira: asters", provenance.Raw)
	var sender sql.NullInt64
	var fromMe sql.NullBool
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sender_id,is_from_me FROM messages WHERE snippet = 'fictional garden note'").Scan(&sender, &fromMe))
	require.True(sender.Valid, "a literal You label is still sender evidence, not a senderless notice")
	assert.False(fromMe.Valid, "a display label cannot establish owner direction")
	var kind string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT identifier_type FROM participant_identifiers WHERE participant_id = ?"), sender.Int64).Scan(&kind))
	assert.Equal("whatsapp_txt", kind)
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sender_id FROM messages WHERE snippet LIKE 'You changed%'").Scan(&sender))
	assert.False(sender.Valid)
	var noticeKey string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_message_id FROM messages WHERE snippet LIKE 'You changed%'").Scan(&noticeKey))
	assert.Equal("whatsapp_txt:a8215365edcf9c7c6082220e6eb0f47f497d2dc4b14c0e9accbaee33b5e2d8bb:0", noticeKey)
}

func TestTextSubsetSenderSearch(t *testing.T) {
	require := require.New(t)
	srcPath := filepath.Join(t.TempDir(), "source.db")
	st := testutil.NewSQLiteTestStoreAt(t, srcPath)
	_, err := importTextForTest(t.Context(), NewImporter(st, nil), textFile(t, "13/6/26, 10:00 - Mira: asters\n"), textOptions())
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'from_addr:Mira'").Scan(&count))
	require.Equal(1, count)
	require.NoError(st.Close())
	dest := t.TempDir()
	_, err = store.CopySubsetWithOptions(t.Context(), srcPath, dest, 10, store.CopySubsetOptions{})
	require.NoError(err)
	subset, err := store.Open(filepath.Join(dest, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = subset.Close() })
	require.NoError(subset.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'from_addr:Mira'").Scan(&count))
	require.Equal(1, count)
}

func TestTextSearchSenderAfterRebuild(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	_, err := importTextForTest(t.Context(), NewImporter(st, nil), textFile(t, "13/6/26, 10:00 - Mira: asters\n"), textOptions())
	require.NoError(err)
	_, total, err := st.SearchMessagesContext(t.Context(), "Mira", 0, 10)
	require.NoError(err)
	require.EqualValues(1, total)
	_, err = st.RebuildFTSContext(t.Context(), nil)
	require.NoError(err)
	_, total, err = st.SearchMessagesContext(t.Context(), "Mira", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
}
