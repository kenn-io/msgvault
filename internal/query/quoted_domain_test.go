package query_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestQuotedLocalPartSenderDomain(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "quoted", "")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant("primary@example.test", "Author", "example.test")
	require.NoError(t, err)
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:    &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "quoted", MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, EmailAddresses: []string{`"a@b"@relay.example.test`}, IsRelayed: true}},
	})
	require.NoError(t, err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	domains, err := engine.Aggregate(t.Context(), query.ViewDomains, query.DefaultAggregateOptions())
	require.NoError(t, err)
	require.Len(t, domains, 1)
	assert.Equal(t, "relay.example.test", domains[0].Key)
	rows, err := engine.ListMessages(t.Context(), query.MessageFilter{Domain: "relay.example.test"})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestRelayedEnvelopeFiltersAndDeletion(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "")
	require.NoError(t, err)
	sender, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET phone_number=? WHERE id=?`), "+12025550199", sender)
	require.NoError(t, err)
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:    &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "relay", MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{"Author"}, EmailAddresses: []string{"relay@service.test"}}},
	})
	require.NoError(t, err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	for _, name := range []string{"", "Author"} {
		for _, tc := range []struct {
			address string
			want    int
		}{{"author@example.test", 0}, {"relay@service.test", 1}, {"+12025550199", 1}} {
			filter := query.MessageFilter{Sender: tc.address, SenderName: name}
			rows, err := engine.ListMessages(t.Context(), filter)
			require.NoError(t, err)
			assert.Len(t, rows, tc.want)
			targets, err := engine.GetDeletionTargetsByFilter(t.Context(), filter)
			require.NoError(t, err)
			assert.Len(t, targets, tc.want)
		}
	}
	for _, tc := range []struct {
		domain string
		want   int64
	}{{"example.test", 0}, {"service.test", 1}} {
		_, count, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("from:@"+tc.domain), 0, 10)
		require.NoError(t, err)
		assert.Equal(t, tc.want, count)
	}
}

func TestRelayedEnvelopeMixedAndLegacyFallback(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "")
	require.NoError(t, err)
	author, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
	require.NoError(t, err)
	second, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	require.NoError(t, err)
	for _, tc := range []struct {
		id         string
		recipients []store.RecipientSet
	}{
		{"mixed", []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{author, second}, EmailAddresses: []string{"relay@service.test", ""}}}},
		{"legacy", []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{author}, EmailAddresses: []string{""}}}},
		{"chat", nil},
	} {
		_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: tc.id, MessageType: "email", SenderID: sql.NullInt64{Int64: author, Valid: true}}, Recipients: tc.recipients})
		require.NoError(t, err)
	}
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	rows, err := engine.ListMessages(t.Context(), query.MessageFilter{Sender: "author@example.test"})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, row := range rows {
		ids[row.SourceMessageID] = true
	}
	assert.Equal(t, map[string]bool{"legacy": true, "chat": true}, ids)
	rows, err = engine.ListMessages(t.Context(), query.MessageFilter{Domain: "example.test"})
	require.NoError(t, err)
	ids = map[string]bool{}
	for _, row := range rows {
		ids[row.SourceMessageID] = true
	}
	assert.Equal(t, map[string]bool{"legacy": true}, ids)
	targets, err := engine.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{Domain: "example.test"})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "legacy", targets[0].SourceMessageID)
	rows, err = engine.ListMessages(t.Context(), query.MessageFilter{Sender: "other@example.test"})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestRelayedSenderThreadParticipants(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "relay-thread", "")
	require.NoError(t, err)
	author, err := st.EnsureParticipantByIdentifier("github", "author", "Author")
	require.NoError(t, err)
	message, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:    &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "relay", MessageType: "email", SenderID: sql.NullInt64{Int64: author, Valid: true}},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{author}, DisplayNames: []string{"Author"}, EmailAddresses: []string{"notifications@github.com"}, IsRelayed: true}},
	})
	require.NoError(t, err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	check := func() {
		t.Helper()
		thread, err := engine.(query.OriginalMessageReader).ListThread(t.Context(), query.ThreadQuery{ID: message})
		require.NoError(t, err)
		require.Len(t, thread.Messages, 1)
		require.Len(t, thread.Messages[0].From, 1)
		assert.Equal(t, "notifications@github.com", thread.Messages[0].From[0].Email)
	}
	check()
	contact, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
	require.NoError(t, err)
	require.NoError(t, st.MergeParticipants(author, contact))
	check()
}

func TestSenderAggregatesAfterParticipantMerge(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "aggregate-thread", "")
	require.NoError(t, err)
	alias, err := st.EnsureParticipant("old@old.test", "Author", "old.test")
	require.NoError(t, err)
	author, err := st.EnsureParticipantByIdentifier("github", "author", "Author")
	require.NoError(t, err)
	contact, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
	require.NoError(t, err)
	for _, row := range []struct {
		id, address string
		participant int64
		relayed     bool
	}{{"ordinary", "old@old.test", alias, false}, {"relay", "notifications@github.com", author, true}} {
		_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:    &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: row.id, MessageType: "email", SenderID: sql.NullInt64{Int64: row.participant, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
			Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{row.participant}, EmailAddresses: []string{row.address}, IsRelayed: row.relayed}},
		})
		require.NoError(t, err)
	}
	require.NoError(t, st.MergeParticipants(alias, contact))
	require.NoError(t, st.MergeParticipants(author, contact))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	for view, want := range map[query.ViewType][]string{
		query.ViewSenders: {"notifications@github.com", "author@example.test"},
		query.ViewDomains: {"github.com", "example.test"},
	} {
		rows, err := engine.Aggregate(t.Context(), view, query.DefaultAggregateOptions())
		require.NoError(t, err)
		var keys []string
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
		assert.ElementsMatch(t, want, keys)
	}
}
