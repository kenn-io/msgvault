package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchMessageIDsQueryPipeOR(t *testing.T) {
	setupRequire := require.New(t)
	f := storetest.New(t)
	ids := make([]int64, 0, 2)
	for i, body := range []string{"alpha beta", "gamma"} {
		id := f.NewMessage().WithSourceMessageID(body).WithSubject("ordinary subject").WithSnippet("ordinary preview").WithSentAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Create(t, f.Store)
		setupRequire.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		ids = append(ids, id)
		if i == 1 {
			setupRequire.NoError(f.Store.MarkMessageDeleted(f.Source.ID, body))
		}
	}
	_, err := f.Store.BackfillFTS(nil)
	setupRequire.NoError(err)
	for _, tc := range []struct {
		scope search.DeletionScope
		want  []int64
	}{
		{search.DeletionScopeAny, ids},
		{search.DeletionScopeActive, ids[:1]},
		{search.DeletionScopeDeleted, ids[1:]},
	} {
		t.Run(string(tc.scope), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			q := search.ParseOR("alpha beta | gamma")
			q.DeletionScope = tc.scope
			got, total, err := f.Store.SearchMessageIDsQueryContext(t.Context(), q, 50)
			require.NoError(err)
			assert.Equal(int64(len(tc.want)), total)
			assert.ElementsMatch(tc.want, got)
		})
	}
	q := search.ParseOR("alpha beta | gamma")
	q.DeletionScope = search.DeletionScopeAny
	rows, total, err := f.Store.SearchMessagesQuery(q, 0, 1)
	setupRequire.NoError(err)
	setupRequire.Len(rows, 1)
	assert.Equal(t, int64(2), total)
	assert.Equal(t, ids[1], rows[0].ID)
	rows, _, err = f.Store.SearchMessagesQuery(q, 1, 1)
	setupRequire.NoError(err)
	setupRequire.Len(rows, 1)
	assert.Equal(t, ids[0], rows[0].ID)
}

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

// TestSearchMessagesQuery_DeletionScope verifies that Query.DeletionScope
// selects which population the full-text search covers relative to source
// deletion. The FTS index keeps entries for source-deleted messages (soft
// deletion only stamps deleted_from_source_at), so scope=deleted must return
// them and scope=any must return everything; the zero value keeps the
// historical active-only behavior. Runs under SQLite and PostgreSQL.
func TestSearchMessagesQuery_DeletionScope(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	live := f.NewMessage().
		WithSourceMessageID("scope-live").
		WithSubject("ledger summary live").
		WithSnippet("quarterly ledger totals").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(live,
		sql.NullString{String: "ledger body live", Valid: true},
		sql.NullString{}), "UpsertMessageBody live")

	deleted := f.NewMessage().
		WithSourceMessageID("scope-deleted").
		WithSubject("ledger summary archived").
		WithSnippet("quarterly ledger totals").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(deleted,
		sql.NullString{String: "ledger body archived", Valid: true},
		sql.NullString{}), "UpsertMessageBody deleted")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")
	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "scope-deleted"),
		"MarkMessageDeleted")

	cases := []struct {
		name    string
		scope   search.DeletionScope
		wantIDs []int64
	}{
		{"zero_value_defaults_to_active", search.DeletionScope(""), []int64{live}},
		{"active", search.DeletionScopeActive, []int64{live}},
		{"deleted", search.DeletionScopeDeleted, []int64{deleted}},
		{"any", search.DeletionScopeAny, []int64{live, deleted}},
		{"unknown_fails_closed_to_active", search.DeletionScope("bogus"), []int64{live}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			msgs, total, err := f.Store.SearchMessagesQuery(&search.Query{
				TextTerms:     []string{"ledger"},
				DeletionScope: tc.scope,
			}, 0, 50)
			require.NoError(err, "scoped search")
			assert.Equal(int64(len(tc.wantIDs)), total, "scoped total")
			gotIDs := make([]int64, 0, len(msgs))
			for _, msg := range msgs {
				gotIDs = append(gotIDs, msg.ID)
			}
			assert.ElementsMatch(tc.wantIDs, gotIDs, "scoped message IDs")
		})
	}
}

// TestSearchMessagesQuery_ConversationIDScoping guards the store API path used
// by callers that bypass the analytics query engine. Multiple IDs are one
// conversation scope, so they match any listed conversation while excluding
// messages from every other thread.
func TestSearchMessagesQuery_ConversationIDScoping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.NewMessage().
		WithSourceMessageID("conversation-scope-first").
		Create(t, f.Store)

	secondConversation, err := f.Store.EnsureConversation(
		f.Source.ID, "conversation-scope-second", "Second Thread",
	)
	require.NoError(err, "EnsureConversation second")
	second := storetest.NewMessage(f.Source.ID, secondConversation).
		WithSourceMessageID("conversation-scope-second").
		Create(t, f.Store)

	thirdConversation, err := f.Store.EnsureConversation(
		f.Source.ID, "conversation-scope-third", "Third Thread",
	)
	require.NoError(err, "EnsureConversation third")
	storetest.NewMessage(f.Source.ID, thirdConversation).
		WithSourceMessageID("conversation-scope-third").
		Create(t, f.Store)

	msgs, total, err := f.Store.SearchMessagesQuery(&search.Query{
		ConversationIDs: []int64{f.ConvID, secondConversation},
	}, 0, 50)
	require.NoError(err, "SearchMessagesQuery")
	assert.Equal(int64(2), total, "scoped total")
	gotIDs := make([]int64, 0, len(msgs))
	for _, msg := range msgs {
		gotIDs = append(gotIDs, msg.ID)
	}
	assert.ElementsMatch([]int64{first, second}, gotIDs, "scoped message IDs")
}

func TestSearchMessagesQuery_ExplicitEmptyConversationScopeMatchesNothing(t *testing.T) {
	f := storetest.New(t)
	f.NewMessage().
		WithSourceMessageID("conversation-scope-must-not-widen").
		Create(t, f.Store)

	msgs, total, err := f.Store.SearchMessagesQuery(&search.Query{
		ConversationIDs: []int64{},
	}, 0, 50)

	require.NoError(t, err, "SearchMessagesQuery")
	assert.Zero(t, total, "explicit empty scope total")
	assert.Empty(t, msgs, "explicit empty scope messages")
}

func TestSearchMessagesPipeOR(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	ids := make([]int64, 0, 2)
	for _, typ := range []string{"sms", "mms"} {
		message := f.NewMessage().WithSourceMessageID(typ).WithSubject("beta").Build()
		message.MessageType = typ
		id, err := f.Store.UpsertMessage(message)
		requirements.NoError(err)
		ids = append(ids, id)
	}
	_, err := f.Store.BackfillFTS(nil)
	requirements.NoError(err)
	t.Run("caller vs branch type", func(t *testing.T) {
		q := search.ParseOR("alpha | beta message_type:mms")
		q.MessageTypes = []string{"sms"}
		rows, total, err := f.Store.SearchMessagesQuery(q, 0, 50)
		require.NoError(t, err)
		assert.Zero(t, total)
		got := make([]int64, len(rows))
		for i, row := range rows {
			got[i] = row.ID
		}
		assert.Empty(t, got)
	})
	t.Run("empty FTS term", func(t *testing.T) {
		rows, total, err := f.Store.SearchMessagesQuery(search.ParseOR("!!! | subject:beta"), 0, 50)
		require.NoError(t, err)
		assert.Equal(t, int64(len(ids)), total)
		assert.Len(t, rows, len(ids))
	})
	t.Run("dropped messages_fts table", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		if f.Store.IsPostgreSQL() {
			t.Skip("messages_fts is SQLite-only")
		}
		_, err := f.Store.DB().Exec("DROP TABLE messages_fts")
		require.NoError(err)
		rows, total, err := f.Store.SearchMessagesQuery(search.ParseOR("alpha | beta"), 0, 50)
		require.NoError(err)
		assert.Equal(int64(len(ids)), total)
		assert.Len(rows, len(ids))
	})
}
