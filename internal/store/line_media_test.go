package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLineMediaPreservesDerivedRevision(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	chat, err := st.EnsureConversationWithType(src.ID, "group:synthetic", "group_chat", "Example")
	must.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, ConversationID: chat, SourceMessageID: "1", MessageType: "line"},
		BodyText: sql.NullString{String: "file", Valid: true},
	})
	must.NoError(err)
	before, err := st.DerivedDataRevisionContext(t.Context())
	must.NoError(err)
	must.NoError(st.UpsertLineAttachmentContext(t.Context(), id, store.AttachmentWrite{
		SourceAttachmentID: "line:1", Filename: "example.bin", ContentHash: "synthetic-hash", StoragePath: "synthetic-path", Size: 12,
		State: attachmentpolicy.StateStored,
	}))
	after, err := st.DerivedDataRevisionContext(t.Context())
	must.NoError(err)
	checks.Equal(before, after, "attachment writes use the message-change journal")
	messages, total, err := st.SearchMessagesQuery(search.Parse("larger:15"), 0, 10)
	must.NoError(err)
	checks.Equal(int64(1), total)
	must.Len(messages, 1)
	checks.Equal(int64(16), messages[0].SizeEstimate)
}
