package identityindex_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/x"
)

func TestXWrittenPostsUseChatClassification(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	page := x.Page{Posts: []x.Post{
		{ID: "101", AuthorID: "1001", ConversationID: "101", Text: "Synthetic root", CreatedAt: at},
		{ID: "102", AuthorID: "1002", ConversationID: "101", Text: "Synthetic reply", CreatedAt: at.Add(time.Minute), References: []x.Reference{{Type: "replied_to", ID: "101"}}},
	}, Users: []x.User{{ID: "1001", ProtectedKnown: true}, {ID: "1002", ProtectedKnown: true}}}
	_testRequire.NoError(x.WritePage(t.Context(), st, source.ID, "1001", page, at))
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT m.source_message_id, m.is_from_me, (`+identityindex.IsChatSQL("m.message_type", "c.conversation_type")+`) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.source_id=? ORDER BY m.source_message_id`), source.ID)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id string
		var owned, chat bool
		_testRequire.NoError(rows.Scan(&id, &owned, &chat))
		ids = append(ids, id)
		_testAssert.True(chat, "written post %s must enter the real identity-index SQL chat population", id)
		_testAssert.Equal(id == "101", owned)
	}
	_testRequire.NoError(rows.Err())
	_testAssert.Equal([]string{"101", "102"}, ids)
}
