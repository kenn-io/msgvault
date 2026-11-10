package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestApplyInboxMessagesArchivesMessagesAndEchoes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
	})
	inbox := Inbox{App: "9", Page: "100"}
	src, err := NewInboxImporter(st, c).Register(t.Context(), inbox)
	require.NoError(err)

	require.NoError(ApplyInboxMessages(t.Context(), st, inbox, feedValues(
		`{"sender":{"id":"700"},"recipient":{"id":"100"},"timestamp":1790000000000,"message":{"mid":"m_1","text":"orchard question"}}`,
		`{"sender":{"id":"100"},"recipient":{"id":"700"},"timestamp":1790000060000,"message":{"mid":"m_2","text":"our answer","is_echo":true}}`,
		`{"sender":{"id":"700"},"recipient":{"id":"100"},"timestamp":1790000120000,"reaction":{"mid":"m_2","action":"react","reaction":"love"}}`,
		`{"sender":{"id":"700"},"recipient":{"id":"100"},"timestamp":1790000180000,"read":{"watermark":1790000060000}}`,
	)))
	question, answer := message(t, st, src.ID, "m_1"), message(t, st, src.ID, "m_2")
	assert.Equal(question.ConversationID, answer.ConversationID)
	assert.False(question.IsFromMe)
	assert.True(answer.IsFromMe, "an echo is the Page's own message")
	var key string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT source_conversation_id FROM conversations WHERE id=?`, question.ConversationID).Scan(&key))
	assert.Equal("700", key, "webhook messages land in the chat polling keys by customer")
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(2, count, "reactions and reads are not messages")
}

// Polling and webhooks write the same chat: a delayed event for a message
// polling already archived keeps the polled text, and a new message from a
// known customer is searchable by the name polling stored.
func TestApplyInboxMessagesAfterPolling(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100/conversations":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"t_1","participants":{"data":[{"id":"700","name":"Orchard Customer"},{"id":"100","name":"Example Page"}]},
				"messages":{"data":[{"id":"m_1","created_time":"2026-10-01T12:00:00+0000","from":{"id":"700","name":"Orchard Customer"},"message":"edited question"}]}}]}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	inbox := Inbox{App: "9", Page: "100"}
	imp := NewInboxImporter(st, c)
	src, err := imp.Register(t.Context(), inbox)
	require.NoError(err)
	_, err = imp.Import(t.Context(), inbox, ImportOptions{})
	require.NoError(err)

	require.NoError(ApplyInboxMessages(t.Context(), st, inbox, feedValues(
		`{"sender":{"id":"700"},"recipient":{"id":"100"},"timestamp":1790000000000,"message":{"mid":"m_1","text":"original question"}}`,
		`{"sender":{"id":"700"},"recipient":{"id":"100"},"timestamp":1790000060123,"message":{"mid":"m_2","text":"follow up"}}`,
		`{"sender":{"id":"700"},"recipient":{"id":"100"},"timestamp":1790000070000,"message":{"mid":"m_3","attachments":[{"type":"image"}]}}`,
	)))
	first, second := message(t, st, src.ID, "m_1"), message(t, st, src.ID, "m_2")
	body, err := st.GetMessageBodyText(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal("edited question", body)
	assert.Equal(first.ConversationID, second.ConversationID)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m_3"})
	require.NoError(err)
	assert.Empty(ids, "a message without text is not archived")
	var hits int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '"Orchard Customer"'`).Scan(&hits))
	assert.Equal(2, hits)
	var sent string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sent_at FROM messages WHERE id=?`, second.ID).Scan(&sent))
	assert.Contains(sent, ".123", "millisecond timestamps are kept")
}
