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

func TestPopulateMessageBodiesContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	text := strings.Repeat("α", 256*1024) + " Your code is 482913"
	_, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "content", "Verification", text)
	requirements.NoError(err)
	const html = "<p>Your code is 482913</p>"
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: text, Valid: true}, sql.NullString{String: html, Valid: true}))
	_, empty, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "empty", "Empty", "")
	requirements.NoError(err)
	msg, err := st.GetMessage(id)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet=? WHERE id=?"), "Your code", id)
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE conversations SET title=? WHERE id=?"), "Account verification", msg.ConversationID)
	requirements.NoError(err)
	requirements.NoError(st.UpsertAttachment(id, "482913.txt", "text/plain", "", strings.Repeat("a", 64), 6))
	rows := []store.APIMessage{{ID: empty}, {ID: id, Subject: "Verification"}, {ID: 99999}}
	requirements.NoError(st.PopulateMessageBodiesContext(context.Background(), rows))
	assertions.Equal(text, rows[1].BodyText)
	assertions.Equal(html, rows[1].BodyHTML)
	assertions.Equal(text, rows[1].Body)
	assertions.Equal("Verification", rows[1].Subject)
	assertions.Equal("Your code", rows[1].Snippet)
	assertions.Equal("Account verification", rows[1].ConversationTitle)
	if assert.Len(t, rows[1].Attachments, 1) {
		assert.Equal(t, "482913.txt", rows[1].Attachments[0].Filename)
	}
	assertions.Empty(rows[0].Body)
	assertions.Empty(rows[2].Body)
	requirements.NoError(st.PopulateMessageBodiesContext(context.Background(), nil))
}
