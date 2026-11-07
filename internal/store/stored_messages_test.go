package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func persistStoredMessage(
	t *testing.T, st *store.Store, sourceID, conversationID int64,
	sourceMessageID string, raw []byte, rawFormat string,
) int64 {
	t.Helper()
	body := sql.NullString{String: "body " + sourceMessageID, Valid: true}
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID:        sourceID,
			SourceMessageID: sourceMessageID,
			ConversationID:  conversationID,
			MessageType:     "whatsapp",
			SentAt:          sql.NullTime{Time: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
			Snippet:         body,
			SizeEstimate:    int64(len(body.String)),
		},
		BodyText:  body,
		RawMIME:   raw,
		RawFormat: rawFormat,
		FTS:       &store.FTSDoc{Body: body.String, FromAddr: "+15555550101"},
	})
	require.NoError(t, err)
	return id
}

func TestStoredMessagesContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("whatsapp", "+15555550100")
	require.NoError(err)
	other, err := st.GetOrCreateSource("whatsapp", "+15555550199")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat-1", "direct_chat", "")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversationWithType(other.ID, "chat-1", "direct_chat", "")
	require.NoError(err)

	raw := []byte(`{"text":"compressed by the store"}`)
	firstID := persistStoredMessage(t, st, source.ID, conversationID, "first", raw, "whatsapp_apple_json")
	persistStoredMessage(t, st, source.ID, conversationID, "other-format", []byte(`{}`), "whatsapp_android_json")
	persistStoredMessage(t, st, other.ID, otherConversationID, "foreign", raw, "whatsapp_apple_json")

	stored, err := st.StoredMessagesContext(
		ctx, source.ID, "whatsapp_apple_json", []string{"first", "other-format", "foreign", "missing"},
	)
	require.NoError(err)
	require.Len(stored, 2)
	first := stored["first"]
	assert.Equal(firstID, first.ID)
	assert.Equal(conversationID, first.ConversationID)
	assert.Equal(raw, first.Raw)
	assert.Equal("whatsapp", first.MessageType)
	assert.Equal("body first", first.Snippet.String)
	assert.True(first.SourceIsFromMe.Valid)
	assert.False(first.SourceIsFromMe.Bool)
	assert.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), first.SentAt.Time.UTC())
	assert.Nil(stored["other-format"].Raw)
	_, foreign := stored["foreign"]
	assert.False(foreign, "another source's message with the same ID is excluded")

	_, err = st.DB().ExecContext(
		t.Context(),
		st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
		[]byte("not zlib"), firstID,
	)
	require.NoError(err)
	corrupt, err := st.StoredMessagesContext(ctx, source.ID, "whatsapp_apple_json", []string{"first"})
	require.NoError(err, "an unreadable payload is a mismatch, not an error")
	assert.Nil(corrupt["first"].Raw)
	assert.Equal(firstID, corrupt["first"].ID)

	ids := make([]string, 0, 501)
	for i := range 501 {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}
	ids = append(ids, "first")
	chunked, err := st.StoredMessagesContext(ctx, source.ID, "whatsapp_apple_json", ids)
	require.NoError(err)
	assert.Len(chunked, 1)
}

func TestMessageContentMatchesContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("whatsapp", "+15555550100")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat-1", "direct_chat", "")
	require.NoError(err)
	id := persistStoredMessage(t, st, source.ID, conversationID, "first", []byte(`{}`), "whatsapp_apple_json")

	body := sql.NullString{String: "body first", Valid: true}
	doc := store.FTSDoc{Body: "body first", FromAddr: "+15555550101"}
	matches, err := st.MessageContentMatchesContext(ctx, id, body, doc)
	require.NoError(err)
	assert.True(matches)

	changes := map[string]func() (sql.NullString, store.FTSDoc){
		"body": func() (sql.NullString, store.FTSDoc) {
			return sql.NullString{String: "edited", Valid: true}, doc
		},
		"search body": func() (sql.NullString, store.FTSDoc) {
			changed := doc
			changed.Body = "edited"
			return body, changed
		},
		"sender": func() (sql.NullString, store.FTSDoc) {
			changed := doc
			changed.FromAddr = "+15555550102"
			return body, changed
		},
		"subject": func() (sql.NullString, store.FTSDoc) {
			changed := doc
			changed.Subject = "subject"
			return body, changed
		},
		"recipients": func() (sql.NullString, store.FTSDoc) {
			changed := doc
			changed.ToAddrs = "+15555550103"
			changed.CcAddrs = "+15555550104"
			return body, changed
		},
	}
	for name, change := range changes {
		changedBody, changedDoc := change()
		matches, err := st.MessageContentMatchesContext(ctx, id, changedBody, changedDoc)
		require.NoError(err, name)
		assert.False(matches, name)
	}

	missing, err := st.MessageContentMatchesContext(ctx, id+1000, body, doc)
	require.NoError(err)
	assert.False(missing, "a message without a body never matches")

	require.NoError(st.UpsertFTS(id, "", "body first", "+15555550199", "", ""))
	stale, err := st.MessageContentMatchesContext(ctx, id, body, doc)
	require.NoError(err)
	assert.False(stale, "a search document naming another sender is stale")

	clearDocument := `DELETE FROM messages_fts WHERE rowid = ?`
	if st.IsPostgreSQL() {
		clearDocument = `UPDATE messages SET search_fts = NULL WHERE id = ?`
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(clearDocument), id)
	require.NoError(err)
	absent, err := st.MessageContentMatchesContext(ctx, id, body, doc)
	require.NoError(err)
	assert.False(absent, "a message without a search document never matches")
}

func TestStoredMessageMatches(t *testing.T) {
	instant := sql.NullTime{Time: time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC), Valid: true}
	message := &store.Message{
		ConversationID: 1, SenderID: sql.NullInt64{Int64: 2, Valid: true},
		IsFromMe: true, MessageType: "sms", SentAt: instant, InternalDate: instant,
		Snippet: sql.NullString{String: "hello", Valid: true}, SizeEstimate: 5,
	}
	stored := store.StoredMessage{
		ConversationID: 1, SenderID: sql.NullInt64{Int64: 2, Valid: true},
		SourceIsFromMe: sql.NullBool{Bool: true, Valid: true}, MessageType: "sms",
		SentAt: instant, InternalDate: instant,
		Snippet: sql.NullString{String: "hello", Valid: true}, SizeEstimate: 5, Raw: []byte(`{"body":"hello"}`),
	}
	for _, tc := range []struct {
		name   string
		change func(*store.StoredMessage)
		want   bool
	}{
		{"same", func(*store.StoredMessage) {}, true},
		{"conversation", func(s *store.StoredMessage) { s.ConversationID++ }, false},
		{"sender", func(s *store.StoredMessage) { s.SenderID.Int64++ }, false},
		{"sender validity", func(s *store.StoredMessage) { s.SenderID.Valid = false }, false},
		{"direction", func(s *store.StoredMessage) { s.SourceIsFromMe.Bool = false }, false},
		{"direction validity", func(s *store.StoredMessage) { s.SourceIsFromMe.Valid = false }, false},
		{"type", func(s *store.StoredMessage) { s.MessageType = "imessage" }, false},
		{"sent date", func(s *store.StoredMessage) { s.SentAt.Time = s.SentAt.Time.Add(time.Second) }, false},
		{"sent date validity", func(s *store.StoredMessage) { s.SentAt.Valid = false }, false},
		{"internal date", func(s *store.StoredMessage) { s.InternalDate.Time = s.InternalDate.Time.Add(time.Second) }, false},
		{"internal date validity", func(s *store.StoredMessage) { s.InternalDate.Valid = false }, false},
		{"snippet", func(s *store.StoredMessage) { s.Snippet.String = "edited" }, false},
		{"snippet validity", func(s *store.StoredMessage) { s.Snippet.Valid = false }, false},
		{"size", func(s *store.StoredMessage) { s.SizeEstimate++ }, false},
		{"raw", func(s *store.StoredMessage) { s.Raw = []byte(`{"body":"edited"}`) }, false},
		{"microsecond precision", func(s *store.StoredMessage) {
			s.SentAt.Time = s.SentAt.Time.Truncate(time.Microsecond)
			s.InternalDate.Time = s.InternalDate.Time.Round(time.Microsecond)
		}, true},
		{"microsecond boundary", func(s *store.StoredMessage) { s.SentAt.Time = s.SentAt.Time.Add(time.Microsecond) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := stored
			tc.change(&candidate)
			assert.Equal(t, tc.want, candidate.Matches(message, []byte(`{"body":"hello"}`)))
		})
	}
}
