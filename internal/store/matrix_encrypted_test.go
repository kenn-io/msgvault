package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMatrixPendingEventsBySession(t *testing.T) {
	req := require.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(t.Context(), source.ID, "!room:example.org", "Room")
	req.NoError(err)
	var ids []int64
	for _, item := range []struct{ event, session string }{{"$a", "session-a"}, {"$b", "session-b"}, {"$c", "session-a"}} {
		id, err := s.PersistMatrixMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: item.event, MessageType: "matrix"},
			RawMIME: []byte(item.event), RawFormat: "matrix_json",
		}, store.MatrixPersist{SessionID: item.session})
		req.NoError(err)
		ids = append(ids, id)
	}

	t.Run("filter", func(t *testing.T) {
		req := require.New(t)
		type sessionLookup interface {
			MatrixPendingEventsBySession(ctx context.Context, conversationID int64, sessionID string, afterID int64, limit int) ([]store.MatrixEncryptedEvent, error)
		}
		lookup, ok := any(s).(sessionLookup)
		req.True(ok, "store must support a session-filtered pending lookup")
		events, err := lookup.MatrixPendingEventsBySession(t.Context(), convID, "session-a", 0, 10)
		req.NoError(err)
		req.Equal([]store.MatrixEncryptedEvent{{MessageID: ids[0], Ciphertext: []byte("$a")}, {MessageID: ids[2], Ciphertext: []byte("$c")}}, events)
		events, err = lookup.MatrixPendingEventsBySession(t.Context(), convID, "session-b", 0, 10)
		req.NoError(err)
		req.Equal([]store.MatrixEncryptedEvent{{MessageID: ids[1], Ciphertext: []byte("$b")}}, events)
		events, err = lookup.MatrixPendingEventsBySession(t.Context(), convID, "session-a", ids[0], 10)
		req.NoError(err)
		req.Equal([]store.MatrixEncryptedEvent{{MessageID: ids[2], Ciphertext: []byte("$c")}}, events)
	})

	t.Run("index", func(t *testing.T) {
		req := require.New(t)
		if s.IsPostgreSQL() {
			t.Skip("EXPLAIN QUERY PLAN is SQLite-only")
		}
		rows, err := s.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT message_id, ciphertext FROM matrix_encrypted_events WHERE conversation_id = ? AND session_id = ? AND NOT recovered AND message_id > ? ORDER BY message_id LIMIT ?`, convID, "session-a", 0, 10)
		req.NoError(err)
		defer func() { _ = rows.Close() }()
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			req.NoError(rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		req.NoError(rows.Err())
		assert.Contains(t, strings.Join(plan, "\n"), "session_id=?")
	})
}

// Archives from releases before matrix_encrypted_events hold placeholders that
// recovery can only find after setup registers them.
func TestInitSchemaRegistersLegacyMatrixPlaceholders(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(t.Context(), source.ID, "!room:example.org", "Room")
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
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "matrix_encrypted_events_v1")
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
