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
