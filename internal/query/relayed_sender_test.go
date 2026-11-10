package query

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func TestRelayedSenderViews(t *testing.T) {
	for _, backend := range []string{"sqlite", "duckdb"} {
		t.Run(backend, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var engine Engine
			var relayID int64
			if backend == "sqlite" {
				st, err := store.OpenForTest(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
				requirements.NoError(err)
				t.Cleanup(func() { _ = st.Close() })
				requirements.NoError(st.InitSchema())
				source, err := st.GetOrCreateSource("gmail", "reader@example.test")
				requirements.NoError(err)
				conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "")
				requirements.NoError(err)
				alias, err := st.EnsureParticipant("old@old.test", "Author", "old.test")
				requirements.NoError(err)
				author, err := st.EnsureParticipantByIdentifier("github", "author", "Author")
				requirements.NoError(err)
				contact, err := st.EnsureParticipant("author@example.test", "Author", "example.test")
				requirements.NoError(err)
				for _, row := range []struct {
					id, address string
					participant int64
					relayed     bool
				}{{"ordinary", "old@old.test", alias, false}, {"relay", "notifications@github.com", author, true}} {
					relayID, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
						Message:    &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: row.id, MessageType: "email", SenderID: sql.NullInt64{Int64: row.participant, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
						Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{row.participant}, EmailAddresses: []string{row.address}, IsRelayed: row.relayed}},
					})
					requirements.NoError(err)
				}
				requirements.NoError(st.MergeParticipants(alias, contact))
				requirements.NoError(st.MergeParticipants(author, contact))
				_, err = st.DB().ExecContext(t.Context(), `UPDATE participants SET phone_number=? WHERE id=?`, "+12025550199", contact)
				requirements.NoError(err)
				engine = NewEngine(st.DB(), false)
				detail, err := engine.GetMessage(t.Context(), relayID)
				requirements.NoError(err)
				requirements.Len(detail.From, 1)
				assertions.Equal("notifications@github.com", detail.From[0].Email)
			} else {
				b := NewTestDataBuilder(t)
				b.AddSource("reader@example.test")
				author := b.AddParticipant("author@example.test", "example.test", "Author")
				ordinary := b.AddMessage(MessageOpt{Subject: "Ordinary", SenderID: &author})
				b.AddRecipientWithEnvelope(ordinary, author, "from", "Author", "old@old.test")
				relayID = b.AddMessage(MessageOpt{Subject: "Relay", SenderID: &author})
				b.AddRecipientWithEnvelope(relayID, author, "from", "Author", "notifications@github.com")
				b.recipients[len(b.recipients)-1].IsRelayed = true
				duck := b.BuildEngine()
				engine = duck
				var name string
				err := duck.db.QueryRowContext(t.Context(), `SELECT from_name FROM v_senders WHERE from_email = 'notifications@github.com'`).Scan(&name)
				requirements.NoError(err)
				assertions.Equal("notifications@github.com", name)
			}
			t.Cleanup(func() { _ = engine.Close() })
			for view, want := range map[ViewType][]string{ViewSenders: {"notifications@github.com", "author@example.test"}, ViewDomains: {"github.com", "example.test"}} {
				rows, err := engine.Aggregate(t.Context(), view, DefaultAggregateOptions())
				requirements.NoError(err)
				var keys []string
				for _, row := range rows {
					keys = append(keys, row.Key)
				}
				assertions.ElementsMatch(want, keys)
			}
			for address, want := range map[string]int{"author@example.test": 1, "notifications@github.com": 1, "old@old.test": 0} {
				rows, err := engine.ListMessages(t.Context(), MessageFilter{Sender: address})
				requirements.NoError(err)
				assertions.Len(rows, want, address)
				targets, err := engine.GetDeletionTargetsByFilter(t.Context(), MessageFilter{Sender: address})
				requirements.NoError(err)
				assertions.Len(targets, want, address)
			}
			for term, want := range map[string]int{"from:notifications@github.com": 1, "from:author@example.test": 1, "from:old@old.test": 0, "from:@old.test": 0, "from:old": 0, "notifications@github.com": 1} {
				rows, err := engine.SearchFast(t.Context(), search.Parse(term), MessageFilter{}, 10, 0)
				requirements.NoError(err)
				assertions.Len(rows, want, term)
			}
			rows, err := engine.ListMessages(t.Context(), MessageFilter{Sender: "notifications@github.com", Domain: "github.com"})
			requirements.NoError(err)
			assertions.Len(rows, 1)
			if backend == "sqlite" {
				rows, err = engine.ListMessages(t.Context(), MessageFilter{Sender: "+12025550199"})
				requirements.NoError(err)
				assertions.Len(rows, 1)
			}
		})
	}
}
