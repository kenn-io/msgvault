package matrix

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestImportedEncryptedEventsPersistPendingPlaceholders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "@archive:example.org", "manual"))
	conv, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "direct_chat", "Example")
	require.NoError(err)
	owner, err := st.EnsureParticipantByIdentifier(SourceType, "@archive:example.org", "Archive")
	require.NoError(err)
	peer, err := st.EnsureParticipantByIdentifier(SourceType, "@member:example.org", "Member")
	require.NoError(err)
	require.NoError(st.ReplaceConversationParticipants(conv, []store.ConversationParticipantRef{
		{ParticipantID: owner, Role: "member"}, {ParticipantID: peer, Role: "member"},
	}))
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	asOf := time.Now().UTC().Truncate(time.Millisecond)
	for i, sender := range []string{"@archive:example.org", "@member:example.org", "@archive:example.org"} {
		kind, content := "m.room.message", `{"msgtype":"m.text","body":"Synthetic contact"}`
		if i > 0 {
			kind, content = "m.room.encrypted", `{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque","session_id":"session","sender_key":"key"}`
		}
		evt := matrixTestEvent(t, fmt.Sprintf(`{"type":%q,"event_id":"$tier-%d","sender":%q,"origin_server_ts":%d,"content":%s}`,
			kind, i, sender, asOf.AddDate(0, 0, i-5).UnixMilli(), content))
		require.NoError(imp.persistEvent(t.Context(), source.ID, conv, evt, &ImportSummary{}))
	}
	ids, err := st.MessageExistsBatch(source.ID, []string{"$tier-0", "$tier-1", "$tier-2"})
	require.NoError(err)
	for _, key := range []string{"$tier-1", "$tier-2"} {
		messageID := ids[key]
		require.NotZero(messageID)
		var snippet string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT snippet FROM messages WHERE id=?`), messageID).Scan(&snippet))
		assert.Equal(store.MatrixEncryptedPlaceholder, snippet)
		var pending bool
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT NOT recovered FROM matrix_encrypted_events WHERE message_id=?`), messageID).Scan(&pending))
		assert.True(pending)
	}
}
