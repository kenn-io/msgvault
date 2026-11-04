package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Archives from releases before matrix_encrypted_events hold placeholders that
// recovery can only find after setup registers them.
func TestInitSchemaRegistersLegacyMatrixPlaceholders(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(source.ID, "!room:example.org", "Room")
	req.NoError(err)
	persist := func(eventID, raw string) int64 {
		messageID, err := s.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:  &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: eventID, MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}},
			BodyText: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}, RawMIME: []byte(raw), RawFormat: "matrix_json",
		})
		req.NoError(err)
		return messageID
	}
	encrypted := `{"type":"m.room.encrypted","event_id":"$encrypted","content":{"algorithm":"m.megolm.v1.aes-sha2","session_id":"session","ciphertext":"opaque"}}`
	encryptedID := persist("$encrypted", encrypted)
	// A plaintext message can contain the placeholder text; it has nothing to recover.
	persist("$typed", `{"type":"m.room.message","event_id":"$typed","content":{"msgtype":"m.text","body":"[encrypted message — keys unavailable]"}}`)
	_, err = s.DB().Exec(s.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "matrix_encrypted_events_v1")
	req.NoError(err)

	req.NoError(s.InitSchemaContext(t.Context()))

	pending, err := s.CountMatrixPending(t.Context(), source.ID, nil, nil)
	req.NoError(err)
	check.Equal(int64(1), pending)
	due, err := s.MatrixDueEncryptedEvents(t.Context(), convID, 0, store.MatrixRecoveryDue{All: true}, 10)
	req.NoError(err)
	req.Len(due, 1)
	check.Equal(encryptedID, due[0].MessageID)
	check.JSONEq(encrypted, string(due[0].Ciphertext))
}
