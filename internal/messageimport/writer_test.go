package messageimport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWriterSharesSendersOfASharedIdentifierType(t *testing.T) {
	st := testutil.NewTestStore(t)

	write := func(sourceIdentifier string, writer func(*store.Store, *store.Source) *Writer) int64 {
		source, err := st.GetOrCreateSourceContext(t.Context(), "example_forge", sourceIdentifier)
		require.NoError(t, err)
		syncID, err := st.StartSyncContext(t.Context(), source.ID, "example_forge")
		require.NoError(t, err)
		scoped := st.ScopedToSync(source.ID, syncID)
		result, err := writer(scoped, source).Write(t.Context(), ImportedMessage{
			ExternalID:   "comment-1",
			MessageType:  "example_forge",
			Conversation: MessageImportConversation{ExternalID: "thread-1"},
			Sender:       MessageImportSender{ID: "1001", Name: "octo"},
			SentAt:       "2026-11-07T10:00:00Z",
			BodyMarkdown: "Looks good.",
		})
		require.NoError(t, err)
		require.NoError(t, scoped.CompleteSyncContext(t.Context(), syncID, ""))
		var senderID int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT sender_id FROM messages WHERE id = ?`), result.MessageID).Scan(&senderID))
		return senderID
	}
	shared := func(st *store.Store, source *store.Source) *Writer {
		return NewWriter(st, source).WithSenderIdentifierType("example_forge")
	}
	scoped := func(st *store.Store, source *store.Source) *Writer {
		return NewWriter(st, source)
	}

	first := write("account-a", shared)
	assert.Equal(t, first, write("account-b", shared), "one account is one participant in every source")
	assert.NotEqual(t, first, write("account-c", scoped), "without the type an id stays inside its source")

	var identifierType, identifierValue string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT identifier_type, identifier_value FROM participant_identifiers WHERE participant_id = ?
	`), first).Scan(&identifierType, &identifierValue))
	assert.Equal(t, "example_forge", identifierType)
	assert.Equal(t, "1001", identifierValue)
}
