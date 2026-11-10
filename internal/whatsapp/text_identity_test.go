package whatsapp

import (
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	var fromMe bool
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sender_id,is_from_me FROM messages WHERE snippet = 'fictional garden note'").Scan(&sender, &fromMe))
	require.True(sender.Valid, "a literal You label is still sender evidence, not a senderless notice")
	assert.False(fromMe, "a display label cannot establish owner direction")
	var kind string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT identifier_type FROM participant_identifiers WHERE participant_id = ?"), sender.Int64).Scan(&kind))
	assert.Equal("whatsapp_txt", kind)
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sender_id FROM messages WHERE snippet LIKE 'You changed%'").Scan(&sender))
	assert.False(sender.Valid)
}

func TestTextPayloadKeyIgnoresClassification(t *testing.T) {
	rows, _, err := parseTextForTest(t.Context(), strings.NewReader(`13/6/26, 10:00 - Mira changed the subject to "Asters: blue"`), "dmy", "UTC", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "notice", rows[0].Kind)
	assert.Equal(t, "whatsapp_txt:32fab6e2ab8d05c10874e3ca0027ad09a69e01277ef712a44169dc2e240ea4d8:0", textMessageKey("garden", rows[0], 0))
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
