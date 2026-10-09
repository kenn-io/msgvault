package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Explore hands account scopes to the lexical candidate search, so they must
// narrow the pool before its limit rather than after.
func TestSearchMessageIDsHonorsAccountScopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	persist := func(key, raw string) int64 {
		id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email", Subject: sql.NullString{String: "invoice", Valid: true}},
			RawMIME: []byte(raw),
		})
		require.NoError(err)
		return id
	}
	persist("other", "Subject: invoice\r\n\r\nbody")
	work := persist("work", "X-Delivered-To: work@example.org\r\nSubject: invoice\r\n\r\nbody")

	q := search.Parse("subject:invoice")
	q.AccountScopes = []search.AccountScope{{Addresses: []string{"work@example.org"}}}
	ids, total, err := st.SearchMessageIDsQueryContext(t.Context(), q, 1)
	require.NoError(err)
	assert.Equal(int64(1), total)
	assert.Equal([]int64{work}, ids)
}
