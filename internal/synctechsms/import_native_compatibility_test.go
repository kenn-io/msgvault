package synctechsms

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// Replacing split writes must preserve omitted text and additive membership.
// Native MMS keys use participant IDs; replay must update the original row.
func TestImportSynctechMMSReplayPreservesOmittedBodyAndMembership(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "messages.xml")
	xml := `<smses count="1"><mms date="1717214460000" msg_box="1" m_id="synthetic-preserved-mms"><parts><part seq="0" ct="text/plain" text="Synthetic retained text" /></parts><addrs><addr address="+15550000002" type="137" /><addr address="+15550000001" type="151" /></addrs></mms></smses>`
	require.NoError(os.WriteFile(path, []byte(xml), 0o600))
	imp := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true})
	first, err := imp.ImportPath(path)
	require.NoError(err)
	require.Len(first.MessageIDs, 1)
	var conversationID int64
	var sourceKey string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id, source_message_id FROM messages WHERE id=?`), first.MessageIDs[0]).Scan(&conversationID, &sourceKey))
	thirdID, err := st.EnsureParticipantByPhone("+15550000003", "Synthetic Member", ParticipantIdentifierType)
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(conversationID, thirdID, "member"))
	xml = `<smses count="1"><mms date="1717214460000" msg_box="1" m_id="synthetic-preserved-mms"><parts></parts><addrs><addr address="+15550000002" type="137" /><addr address="+15550000001" type="151" /></addrs></mms></smses>`
	require.NoError(os.WriteFile(path, []byte(xml), 0o600))
	second, err := imp.ImportPath(path)
	require.NoError(err)
	assert.Equal(first.MessageIDs, second.MessageIDs)
	var body sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), first.MessageIDs[0]).Scan(&body))
	assert.Equal(sql.NullString{String: "Synthetic retained text", Valid: true}, body)
	var key string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages WHERE id=?`), first.MessageIDs[0]).Scan(&key))
	assert.Equal(sourceKey, key)
	var members int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), conversationID).Scan(&members))
	assert.Equal(3, members)
}

// Synctech's nonphone addresses are service identifiers, including emails.
// Using generic email resolution during atomic conversion changes attribution.
func TestImportSynctechPreservesNativeNonphoneIdentifierNamespace(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "messages.xml")
	require.NoError(os.WriteFile(path, []byte(`<smses count="1"><sms address="sender@example.test" date="1717214400000" type="1" body="Synthetic namespace" /></smses>`), 0o600))
	summary, err := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeSMS: true}).ImportPath(path)
	require.NoError(err)
	require.Len(summary.MessageIDs, 1)
	var senderID, identifierID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), summary.MessageIDs[0]).Scan(&senderID))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM participant_identifiers WHERE identifier_type=? AND identifier_value=?`), ParticipantIdentifierType, "sender@example.test").Scan(&identifierID))
	assert.Equal(identifierID, senderID)
	var email sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT email_address FROM participants WHERE id=?`), senderID).Scan(&email))
	assert.False(email.Valid)
}
