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

// TestSearchMessagesQuery_TokenlessTextTerms verifies that text terms which
// reduce to nothing usable under the FTS tokenizer ("!!!", "---", "")
// neither error nor short-circuit through the FTS function. PG's
// to_tsquery('simple', ”) raises "text-search query doesn't contain
// lexemes" and SQLite's FTS5 MATCH on an empty/punctuation-only string is
// a syntax error; the store now substitutes a FALSE condition so the
// query returns zero rows from any backend without ever building a
// malformed FTS argument. Runs under both SQLite and PostgreSQL.
func TestSearchMessagesQuery_TokenlessTextTerms(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// Seed two messages with real searchable content so the test would
	// see a non-zero baseline if the FTS predicate were dropped instead
	// of replaced with FALSE.
	msg1 := f.NewMessage().
		WithSourceMessageID("search-msg-1").
		WithSubject("invoice attached").
		WithSnippet("see the attached invoice").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "invoice body text", Valid: true},
		sql.NullString{}), "UpsertMessageBody 1")

	msg2 := f.NewMessage().
		WithSourceMessageID("search-msg-2").
		WithSubject("project update").
		WithSnippet("weekly status").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "project body text", Valid: true},
		sql.NullString{}), "UpsertMessageBody 2")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	// Sanity: a real term must still match — proves the test setup is
	// wired correctly and isn't accidentally returning zero for everything.
	msgs, total, err := f.Store.SearchMessagesQuery(
		&search.Query{TextTerms: []string{"invoice"}}, 0, 50,
	)
	require.NoError(err, "baseline search")
	require.GreaterOrEqual(total, int64(1), "baseline search 'invoice' returned %d hits", total)
	require.GreaterOrEqual(len(msgs), 1)

	// Each of these reduces to no usable tokens. Must not error and
	// must return zero rows (FALSE predicate substituted by the caller).
	cases := []struct {
		name  string
		terms []string
	}{
		{"only_punctuation", []string{"!!!"}},
		{"only_dashes", []string{"---"}},
		{"empty_string", []string{""}},
		{"mixed_all_empty", []string{"!!!", "---", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, total, err := f.Store.SearchMessagesQuery(
				&search.Query{TextTerms: tc.terms}, 0, 50,
			)
			require.NoError(err, "SearchMessagesQuery(%v)", tc.terms)
			assert.Equal(t, int64(0), total, "total (FALSE predicate should match nothing)")
			assert.Empty(t, msgs)
		})
	}
}

// TestSearchMessages_LegacyRawString verifies the legacy SearchMessages
// entrypoint (raw-string FTS query) sanitizes its input through the
// dialect's BuildFTSArg pipeline. Previously it bound the raw string
// straight into FTSSearchClause's placeholder, so any whitespace or
// metacharacter in a user search would reach to_tsquery on PG (parser
// error) or FTS5 MATCH on SQLite (syntax error). Routing through
// SearchMessagesQuery shares the same FALSE fallback as
// TokenlessTextTerms and lets multi-word queries actually work.
func TestSearchMessages_LegacyRawString(t *testing.T) {
	f := storetest.New(t)

	msg1 := f.NewMessage().
		WithSourceMessageID("legacy-msg-1").
		WithSubject("urgent invoice").
		WithSnippet("please review").
		Create(t, f.Store)
	require.NoError(t, f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "invoice body for review", Valid: true},
		sql.NullString{}), "UpsertMessageBody 1")

	msg2 := f.NewMessage().
		WithSourceMessageID("legacy-msg-2").
		WithSubject("project plan").
		WithSnippet("status update").
		Create(t, f.Store)
	require.NoError(t, f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "project plan body", Valid: true},
		sql.NullString{}), "UpsertMessageBody 2")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(t, err, "BackfillFTS")

	// Multi-word query was the canonical PG failure: "invoice review"
	// fed straight into to_tsquery would error. Now it tokenizes into
	// two terms AND'd by the dialect helper.
	t.Run("multi_word_match", func(t *testing.T) {
		msgs, total, err := f.Store.SearchMessages("invoice review", 0, 50)
		require.NoError(t, err, "SearchMessages('invoice review')")
		require.GreaterOrEqual(t, total, int64(1), "expected >= 1 hit for 'invoice review'")
		require.GreaterOrEqual(t, len(msgs), 1)
	})

	// Single-word query still works.
	t.Run("single_word_match", func(t *testing.T) {
		msgs, total, err := f.Store.SearchMessages("project", 0, 50)
		require.NoError(t, err, "SearchMessages('project')")
		require.GreaterOrEqual(t, total, int64(1), "expected >= 1 hit for 'project'")
		require.GreaterOrEqual(t, len(msgs), 1)
	})

	// Each of these reduces to no usable tokens after splitting on
	// whitespace and per-dialect sanitization. Must not error.
	cases := []struct {
		name  string
		query string
	}{
		{"only_punctuation", "!!!"},
		{"only_dashes", "---"},
		{"whitespace_only", "   \t  "},
		{"mixed_punctuation", "!!! --- ???"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, total, err := f.Store.SearchMessages(tc.query, 0, 50)
			require.NoError(t, err, "SearchMessages(%q)", tc.query)
			assert.Equal(t, int64(0), total)
			assert.Empty(t, msgs)
		})
	}
}
func TestSearchMessagesQuery_MessageTypeFilter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	emailMsg := f.NewMessage().
		WithSourceMessageID("message-type-email").
		WithSubject("lunch plans").
		WithSnippet("grab tacos").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(emailMsg,
		sql.NullString{String: "lunch tacos", Valid: true},
		sql.NullString{}), "UpsertMessageBody email")

	smsMsg := f.NewMessage().
		WithSourceMessageID("message-type-sms").
		WithSubject("lunch plans").
		WithSnippet("grab sushi").
		Create(t, f.Store)
	_, err := f.Store.DB().Exec(
		f.Store.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`),
		"sms", smsMsg)
	require.NoError(err, "mark sms")
	require.NoError(f.Store.UpsertMessageBody(smsMsg,
		sql.NullString{String: "lunch sushi", Valid: true},
		sql.NullString{}), "UpsertMessageBody sms")

	_, err = f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	msgs, total, err := f.Store.SearchMessagesQuery(search.Parse("message_type=sms lunch"), 0, 50)
	require.NoError(err, "SearchMessagesQuery")
	require.Equal(int64(1), total, "total")
	require.Len(msgs, 1, "messages")
	assert.Equal("sms", msgs[0].MessageType, "MessageType")
	assert.Equal(smsMsg, msgs[0].ID, "ID")
}

// TestSearchMessagesQuery_MessageTypeEmailIncludesLegacyBlankRows pins the
// blank-type half of the email filter: Gmail messages imported before
// message_type existed carry an empty value and must still answer
// message_type:email, or narrowing a deletion search by type silently drops
// exactly the oldest mail.
func TestSearchMessagesQuery_MessageTypeEmailIncludesLegacyBlankRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	typed := f.NewMessage().
		WithSourceMessageID("typed-email").
		WithSubject("receipt for lunch").
		WithSnippet("typed").
		Create(t, f.Store)
	legacyBlank := f.NewMessage().
		WithSourceMessageID("legacy-blank-email").
		WithSubject("receipt for lunch").
		WithSnippet("legacy blank").
		Create(t, f.Store)
	sms := f.NewMessage().
		WithSourceMessageID("typed-sms").
		WithSubject("receipt for lunch").
		WithSnippet("text message").
		Create(t, f.Store)

	for id, value := range map[int64]string{legacyBlank: "", sms: "sms"} {
		_, err := f.Store.DB().Exec(
			f.Store.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`), value, id)
		require.NoError(err, "set message_type for %d", id)
	}
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	msgs, total, err := f.Store.SearchMessagesQuery(search.Parse("message_type:email receipt"), 0, 50)
	require.NoError(err, "SearchMessagesQuery")
	got := make([]int64, 0, len(msgs))
	for _, msg := range msgs {
		got = append(got, msg.ID)
	}
	assert.ElementsMatch([]int64{typed, legacyBlank}, got, "legacy blank-typed rows match message_type:email")
	assert.Equal(int64(2), total, "total")
	assert.NotContains(got, sms, "typed non-email rows stay excluded")
}

// TestSearchMessagesQuery_MessageTypeEmailIncludesLegacyNullRows pins the
// NULL half of the same legacy contract: archives written before the NOT
// NULL constraint carry NULL message_type, and message_type:email must match
// them exactly as the repair and analytical paths already do.
func TestSearchMessagesQuery_MessageTypeEmailIncludesLegacyNullRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)

	typed, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID,
		SourceMessageID: "typed-email-null-fixture",
		Subject:         sql.NullString{String: "receipt for lunch", Valid: true},
		Snippet:         sql.NullString{String: "typed", Valid: true}, MessageType: store.MessageTypeEmail,
	})
	require.NoError(err, "create typed email")
	legacyNull, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID,
		SourceMessageID: "legacy-null-email",
		Subject:         sql.NullString{String: "receipt for lunch", Valid: true},
		Snippet:         sql.NullString{String: "legacy null", Valid: true}, MessageType: store.MessageTypeEmail,
	})
	require.NoError(err, "create legacy email")
	_, err = st.DB().Exec(st.Rebind(
		`UPDATE messages SET message_type = NULL WHERE id = ?`), legacyNull)
	require.NoError(err, "clear legacy message type")
	_, err = st.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	msgs, total, err := st.SearchMessagesQuery(search.Parse("message_type:email receipt"), 0, 50)
	require.NoError(err, "SearchMessagesQuery")
	got := make([]int64, 0, len(msgs))
	for _, msg := range msgs {
		got = append(got, msg.ID)
	}
	assert.ElementsMatch([]int64{typed, legacyNull}, got,
		"legacy NULL-typed rows match message_type:email")
	assert.Equal(int64(2), total, "total")
}

// TestSearchMessagesQuery_ListIDFilters catches Store searches that treat
// List-Id substrings as wildcard patterns, skip case folding, or OR repeated
// filters instead of requiring every requested literal substring.
func TestSearchMessagesQuery_ListIDFilters(t *testing.T) {
	f := storetest.New(t)

	create := func(sourceMessageID, listID string) int64 {
		message := f.NewMessage().WithSourceMessageID(sourceMessageID).Build()
		if listID != "" {
			message.ListID = sql.NullString{String: listID, Valid: true}
		}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(t, err, "insert %s", sourceMessageID)
		return id
	}

	alerts := create("list-alerts", "<Alerts.EXAMPLE.test>")
	unicode := create("list-unicode", "<ÉCOLE.example.test>")
	literal := create("list-literal", `<token%_\literal.example.test>`)
	_ = create("list-percent-decoy", `<tokenAA_\literal.example.test>`)
	_ = create("list-underscore-decoy", `<token%AA\literal.example.test>`)
	_ = create("list-escape-decoy", "<token%_AAliteral.example.test>")
	_ = create("list-and-decoy", "<alerts.invalid.test>")
	_ = create("list-null", "")

	cases := []struct {
		name  string
		query string
		want  []int64
	}{
		{name: "case insensitive substring", query: "list:alerts.example", want: []int64{alerts}},
		{name: "Unicode case insensitive substring", query: "list:école", want: []int64{unicode}},
		{name: "literal wildcards and escape character", query: `list:%_\literal`, want: []int64{literal}},
		{name: "repeated aliases are ANDed", query: "list:alerts list-id:example.test", want: []int64{alerts}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			msgs, total, err := f.Store.SearchMessagesQuery(search.Parse(tc.query), 0, 50)
			require.NoError(err, "SearchMessagesQuery")
			require.Equal(int64(len(tc.want)), total, "total")
			require.Len(msgs, len(tc.want), "messages")
			got := make([]int64, len(msgs))
			for i, msg := range msgs {
				got[i] = msg.ID
			}
			assert.ElementsMatch(t, tc.want, got, "matching message IDs")
		})
	}
}
