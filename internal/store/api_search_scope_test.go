package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestSearchMessagesQuery_AccountScoping verifies that Query.AccountIDs
// restricts search results to the given source IDs. The HTTP search
// endpoints resolve an account/collection to source IDs and set this field;
// before the fix searchMessagesQueryImpl never referenced it, so every
// account returned byte-identical results. Runs under SQLite and PostgreSQL.
func TestSearchMessagesQuery_AccountScoping(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// Source 1 (the fixture default) gets one matching message.
	msg1 := f.NewMessage().
		WithSourceMessageID("scope-src1").
		WithSubject("quarterly invoice").
		WithSnippet("please review the invoice").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "invoice body one", Valid: true},
		sql.NullString{}), "UpsertMessageBody src1")

	// Source 2 gets a message with identical searchable content.
	src2, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
	require.NoError(err, "GetOrCreateSource src2")
	conv2, err := f.Store.EnsureConversation(src2.ID, "scope-thread-2", "Scope Thread 2")
	require.NoError(err, "EnsureConversation src2")

	m2 := storetest.NewMessage(src2.ID, conv2).
		WithSourceMessageID("scope-src2").
		WithSubject("quarterly invoice").
		WithSnippet("please review the invoice").
		Build()
	msg2, err := f.Store.UpsertMessage(m2)
	require.NoError(err, "UpsertMessage src2")
	require.NoError(f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "invoice body two", Valid: true},
		sql.NullString{}), "UpsertMessageBody src2")

	_, err = f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	// Baseline: unscoped search sees both messages.
	_, total, err := f.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"invoice"}}, 0, 50,
	)
	require.NoError(err, "unscoped search")
	require.Equal(int64(2), total, "unscoped search should match both sources")

	cases := []struct {
		name      string
		accounts  []int64
		wantTotal int64
		wantID    int64
	}{
		{"source1_only", []int64{f.Source.ID}, 1, msg1},
		{"source2_only", []int64{src2.ID}, 1, msg2},
		{"both_sources", []int64{f.Source.ID, src2.ID}, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			msgs, total, err := f.Store.SearchMessagesQuery(&search.Query{
				TextTerms:  []string{"invoice"},
				AccountIDs: tc.accounts,
			}, 0, 50)
			require.NoError(err, "scoped search")
			assert.Equal(tc.wantTotal, total, "scoped total")
			assert.Len(msgs, int(tc.wantTotal), "scoped result count")
			if tc.wantID != 0 {
				require.Len(msgs, 1)
				assert.Equal(tc.wantID, msgs[0].ID, "scoped message ID")
			}
		})
	}
}
