package messageimport

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWriterSharesSendersOfASharedIdentifierType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	write := func(sourceIdentifier string, writer func(*store.Store, *store.Source) *Writer) int64 {
		source, err := st.GetOrCreateSourceContext(t.Context(), "example_forge", sourceIdentifier)
		require.NoError(err)
		syncID, err := st.StartSyncContext(t.Context(), source.ID, "example_forge")
		require.NoError(err)
		scoped := st.ScopedToSync(source.ID, syncID)
		result, err := writer(scoped, source).Write(t.Context(), ImportedMessage{
			ExternalID:   "comment-1",
			MessageType:  "example_forge",
			Conversation: MessageImportConversation{ExternalID: "thread-1"},
			Sender:       MessageImportSender{ID: "1001", Name: "octo"},
			SentAt:       "2026-11-07T10:00:00Z",
			BodyMarkdown: "Looks good.",
		})
		require.NoError(err)
		require.NoError(scoped.CompleteSyncContext(t.Context(), syncID, ""))
		var senderID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
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
	assert.Equal(first, write("account-b", shared), "one account is one participant in every source")
	assert.NotEqual(first, write("account-c", scoped), "without the type an id stays inside its source")

	var identifierType, identifierValue string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT identifier_type, identifier_value FROM participant_identifiers WHERE participant_id = ?
	`), first).Scan(&identifierType, &identifierValue))
	assert.Equal("example_forge", identifierType)
	assert.Equal("1001", identifierValue)
}

// startWriter starts a sync run of a source of type example_forge and
// returns a writer scoped to it, as a connector's sync builds one.
func startWriter(t *testing.T, st *store.Store, sourceIdentifier string) *Writer {
	t.Helper()
	require := require.New(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), "example_forge", sourceIdentifier)
	require.NoError(err)
	syncID, err := st.StartSyncContext(t.Context(), source.ID, "example_forge")
	require.NoError(err)
	return NewWriter(st.ScopedToSync(source.ID, syncID), source)
}

func examplePost(externalID string, sender MessageImportSender) ImportedMessage {
	return ImportedMessage{
		ExternalID:   externalID,
		MessageType:  "example_forge",
		Conversation: MessageImportConversation{ExternalID: "thread-1", Title: "Add widgets"},
		Sender:       sender,
		SentAt:       "2026-11-07T10:00:00Z",
		BodyMarkdown: "Looks good.",
	}
}

func TestWriterWriteFromKeepsTheConnectorsEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	writer := startWriter(t, st, "account-a").WithConversationType("thread")

	evidence := Evidence{Raw: []byte(`{"id":7,"body":"Looks good."}`), Format: "example_forge_json"}
	result, err := writer.WriteFrom(t.Context(), examplePost("comment-1", MessageImportSender{ID: "1001", Name: "octo"}), evidence)
	require.NoError(err)
	assert.Equal(WriteResult{MessageID: result.MessageID, Created: true, Changed: true}, result)

	raw, err := st.GetMessageRawContext(t.Context(), result.MessageID)
	require.NoError(err)
	assert.Equal(evidence.Raw, raw)
	var rawFormat, conversationType, bodyText string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT mr.raw_format, c.conversation_type, mb.body_text
		FROM messages m
		JOIN message_raw mr ON mr.message_id = m.id
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id = ?
	`), result.MessageID).Scan(&rawFormat, &conversationType, &bodyText))
	assert.Equal("example_forge_json", rawFormat)
	assert.Equal("thread", conversationType)
	assert.Equal("Looks good.", bodyText, "the message is stored from the normalized post")
}

func TestWriterDefaultsToTheImportConversationType(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	result, err := startWriter(t, st, "account-a").Write(t.Context(), examplePost("comment-1", MessageImportSender{Name: "octo"}))
	require.NoError(err)
	var conversationType, rawFormat string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT c.conversation_type, mr.raw_format
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE m.id = ?
	`), result.MessageID).Scan(&conversationType, &rawFormat))
	assert.Equal(t, ConversationType, conversationType)
	assert.Equal(t, RawFormat, rawFormat)
}

func TestWriterWriteFromWritesOnlyChangedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	writer := startWriter(t, st, "account-a")
	post := examplePost("comment-1", MessageImportSender{ID: "1001", Name: "octo"})
	evidence := Evidence{Raw: []byte(`{"id":7,"body":"Looks good."}`), Format: "example_forge_json"}

	first, err := writer.WriteFrom(t.Context(), post, evidence)
	require.NoError(err)
	sentinel := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET last_modified = ? WHERE id = ?`), sentinel, first.MessageID)
	require.NoError(err)
	lastModified := func() string {
		var value string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`), first.MessageID).Scan(&value))
		return value
	}
	before := lastModified()

	again, err := writer.WriteFrom(t.Context(), post, Evidence{Raw: []byte(`{"id":7,"body":"Looks good."}`), Format: "example_forge_json"})
	require.NoError(err)
	assert.Equal(WriteResult{MessageID: first.MessageID}, again)
	assert.Equal(before, lastModified(), "identical evidence writes nothing")

	post.BodyMarkdown = "Looks great."
	edited := Evidence{Raw: []byte(`{"id":7,"body":"Looks great."}`), Format: "example_forge_json"}
	updated, err := writer.WriteFrom(t.Context(), post, edited)
	require.NoError(err)
	assert.Equal(WriteResult{MessageID: first.MessageID, Changed: true}, updated)
	raw, err := st.GetMessageRawContext(t.Context(), first.MessageID)
	require.NoError(err)
	assert.Equal(edited.Raw, raw)
	var bodyText string
	var messages int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT body_text FROM message_bodies WHERE message_id = ?`), first.MessageID).Scan(&bodyText))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal("Looks great.", bodyText)
	assert.Equal(1, messages, "changed evidence updates the same message")
}

func TestWriterWriteFromRejectsUnusableEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence Evidence
	}{
		{"empty raw", Evidence{Format: "example_forge_json"}},
		{"no format", Evidence{Raw: []byte(`{}`)}},
		{"uppercase format", Evidence{Raw: []byte(`{}`), Format: "Example_JSON"}},
		{"hyphenated format", Evidence{Raw: []byte(`{}`), Format: "example-json"}},
		{"leading digit", Evidence{Raw: []byte(`{}`), Format: "1json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			_, err := startWriter(t, st, "account-a").WriteFrom(t.Context(),
				examplePost("comment-1", MessageImportSender{Name: "octo"}), tc.evidence)
			require.Error(t, err)
			var messages int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Zero(t, messages)
		})
	}
}

func TestWriterJoinsAnAccountToItsMailParticipant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// The person's participant from their mail, archived by another source.
	mail, err := st.EnsureParticipantContext(t.Context(), "octo@example.com", "Octo Cat", "example.com")
	require.NoError(err)
	writer := startWriter(t, st, "account-a").WithSenderIdentifierType("example_forge")
	write := func(externalID string, sender MessageImportSender) int64 {
		result, err := writer.Write(t.Context(), examplePost(externalID, sender))
		require.NoError(err)
		return result.MessageID
	}
	senderOf := func(messageID int64) int64 {
		var senderID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&senderID))
		return senderID
	}

	earlier := write("comment-1", MessageImportSender{ID: "1001", Name: "octo"})
	accountOnly := senderOf(earlier)
	require.NotEqual(mail, accountOnly, "without an email the account is its own participant")

	withEmail := write("comment-2", MessageImportSender{ID: "1001", Name: "octo", Email: "octo@example.com"})
	assert.Equal(mail, senderOf(withEmail), "a sender with an email is the email's participant")
	assert.Equal(mail, senderOf(earlier), "the account's earlier participant is merged into it")
	var remaining int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM participants WHERE id = ?`), accountOnly).Scan(&remaining))
	assert.Zero(remaining)
	owner, _, err := st.ParticipantByIdentifierContext(t.Context(), "example_forge", "1001")
	require.NoError(err)
	assert.Equal(mail, owner)

	later := write("comment-3", MessageImportSender{ID: "1001", Name: "octo"})
	assert.Equal(mail, senderOf(later), "a later post naming the account alone reaches the same participant")
}

func TestWriterKeepsParticipantsOfDifferentPeopleApart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	mail, err := st.EnsureParticipantContext(t.Context(), "octo@example.com", "Octo Cat", "example.com")
	require.NoError(err)
	writer := startWriter(t, st, "account-a").WithSenderIdentifierType("example_forge")
	earlier, err := writer.Write(t.Context(), examplePost("comment-1", MessageImportSender{ID: "1001", Name: "octo"}))
	require.NoError(err)
	var accountOnly int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), earlier.MessageID).Scan(&accountOnly))
	// The user keeps the two participants as different people.
	for _, participantID := range []int64{mail, accountOnly} {
		_, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
		require.NoError(err)
	}

	withEmail, err := writer.Write(t.Context(), examplePost("comment-2", MessageImportSender{ID: "1001", Name: "octo", Email: "octo@example.com"}))
	require.NoError(err, "a merge the user's people forbid does not fail the write")
	var earlierSender, laterSender int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), earlier.MessageID).Scan(&earlierSender))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), withEmail.MessageID).Scan(&laterSender))
	assert.Equal(accountOnly, earlierSender, "the earlier participant stays as it is")
	assert.Equal(mail, laterSender)
	owner, _, err := st.ParticipantByIdentifierContext(t.Context(), "example_forge", "1001")
	require.NoError(err)
	assert.Equal(mail, owner, "the account id moves to the sender with the email")
}
