package query

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/senderclass"
	"go.kenn.io/msgvault/internal/store"
)

func TestSenderClassDuckDBSearchAndCachedPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "thread1", "Test Thread")
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirements.NoError(err)
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message:    &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "msg1", MessageType: "email", Subject: sql.NullString{String: "Hello World", Valid: true}},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, EmailAddresses: []string{"alice@example.com"}}},
	})
	requirements.NoError(err)
	requirements.Equal(int64(1), id, "the archive metadata matches the Parquet fixture's first row")
	raw := func(result string) []byte {
		return []byte("From: alice@example.com\r\nAuthentication-Results: mx.test; dmarc=" + result + " header.from=example.com\r\n\r\nBody")
	}
	requirements.NoError(st.UpsertMessageRaw(id, raw("pass")))
	e := newParquetEngine(t)
	e.sqliteEngine = NewSQLiteEngine(st.DB())
	ConfigureSenderAuth(e, map[int64]senderauth.TrustSet{source.ID: {"mx.test"}})
	ConfigureSenderClass(e, st)
	first := func(messages []MessageSummary) senderclass.Readout {
		t.Helper()
		for _, message := range messages {
			if message.ID == id {
				return message.Readout
			}
		}
		require.FailNow(t, "first message missing from search")
		return senderclass.Readout{}
	}
	q := search.Parse("Hello")
	rows, err := e.SearchFast(t.Context(), q, MessageFilter{}, 10, 0)
	requirements.NoError(err)
	assertions.Equal(senderclass.Verified, first(rows).SenderClass)
	page, err := e.SearchFastWithStats(t.Context(), q, "Hello", MessageFilter{}, ViewNoStats, 10, 0)
	requirements.NoError(err)
	assertions.Equal(senderclass.Verified, first(page.Messages).SenderClass)
	_, _, err = st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	page, err = e.SearchFastWithStats(t.Context(), q, "Hello", MessageFilter{}, ViewNoStats, 10, 0)
	requirements.NoError(err)
	assertions.Equal(senderclass.Readout{SenderClass: senderclass.Contact, SenderClassBasis: senderclass.CuratedPerson, SenderRelationship: senderclass.RelationshipContact}, first(page.Messages), "cached hits re-read relationship evidence")
	requirements.NoError(st.UpsertMessageRaw(id, raw("fail")))
	page, err = e.SearchFastWithStats(t.Context(), q, "Hello", MessageFilter{}, ViewNoStats, 10, 0)
	requirements.NoError(err)
	assertions.Equal(senderclass.Readout{SenderClass: senderclass.Unverified, SenderClassBasis: senderclass.AuthFailed, SenderRelationship: senderclass.RelationshipContact}, first(page.Messages), "cached hits re-read authentication before deriving class")
}
