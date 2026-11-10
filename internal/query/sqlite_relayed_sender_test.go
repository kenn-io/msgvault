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

func TestRelayedSenderAfterParticipantMerge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "relay-thread", "")
	requirements.NoError(err)
	alias, err := st.EnsureParticipant("old@old.test", "Author", "old.test")
	requirements.NoError(err)
	author, err := st.EnsureParticipantByIdentifier("github", "author", "Author")
	requirements.NoError(err)
	contact, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
	requirements.NoError(err)
	var relay int64
	for _, row := range []struct {
		id, address string
		participant int64
		relayed     bool
	}{{"ordinary", "old@old.test", alias, false}, {"relay", "notifications@github.com", author, true}} {
		relay, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:    &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: row.id, MessageType: "email", SenderID: sql.NullInt64{Int64: row.participant, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
			Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{row.participant}, EmailAddresses: []string{row.address}, IsRelayed: row.relayed}},
		})
		requirements.NoError(err)
	}
	requirements.NoError(st.MergeParticipants(alias, contact))
	requirements.NoError(st.MergeParticipants(author, contact))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET phone_number=? WHERE id=?`), "+12025550199", contact)
	requirements.NoError(err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	reader, ok := engine.(query.OriginalMessageReader)
	requirements.True(ok)
	thread, err := reader.ListThread(t.Context(), query.ThreadQuery{ID: relay})
	requirements.NoError(err)
	var from []string
	for _, message := range thread.Messages {
		requirements.Len(message.From, 1)
		from = append(from, message.From[0].Email)
	}
	assertions.ElementsMatch([]string{"author@example.test", "notifications@github.com"}, from)
	rows, err := engine.ListMessages(t.Context(), query.MessageFilter{})
	requirements.NoError(err)
	var listed []string
	for _, row := range rows {
		listed = append(listed, row.FromEmail)
	}
	assertions.ElementsMatch([]string{"author@example.test", "notifications@github.com"}, listed)
	for view, want := range map[query.ViewType][]string{
		query.ViewSenders: {"notifications@github.com", "author@example.test"},
		query.ViewDomains: {"github.com", "example.test"},
	} {
		rows, err := engine.Aggregate(t.Context(), view, query.DefaultAggregateOptions())
		requirements.NoError(err)
		var keys []string
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
		assertions.ElementsMatch(want, keys)
	}
	for address, want := range map[string]int{"author@example.test": 0, "notifications@github.com": 1, "+12025550199": 2} {
		filter := query.MessageFilter{Sender: address}
		rows, err := engine.ListMessages(t.Context(), filter)
		requirements.NoError(err)
		assertions.Len(rows, want, address)
		targets, err := engine.GetDeletionTargetsByFilter(t.Context(), filter)
		requirements.NoError(err)
		assertions.Len(targets, want, address)
	}
	for domain, want := range map[string]int64{"example.test": 0, "github.com": 1} {
		_, count, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("from:@"+domain), 0, 10)
		requirements.NoError(err)
		assertions.Equal(want, count, domain)
	}
}
