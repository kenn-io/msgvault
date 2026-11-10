package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuckDBSenderViewsAfterParticipantMerge(t *testing.T) {
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	b.AddSource("reader@example.test")
	author := b.AddParticipant("author@example.test", "example.test", "Author")
	ordinary := b.AddMessage(MessageOpt{Subject: "Ordinary", SenderID: &author})
	b.AddRecipientWithEnvelope(ordinary, author, "from", "Author", "old@old.test")
	relay := b.AddMessage(MessageOpt{Subject: "Relay", SenderID: &author})
	b.AddRecipientWithEnvelope(relay, author, "from", "Author", "notifications@github.com")
	b.recipients[len(b.recipients)-1].IsRelayed = true
	engine := b.BuildEngine()
	t.Cleanup(func() { _ = engine.Close() })
	for _, statement := range []string{
		`SELECT from_email, from_domain FROM v_messages`,
		`SELECT from_email, from_domain FROM v_senders`,
		`SELECT sender_identifier, sender_domain FROM analytical_entries`,
	} {
		func() {
			rows, err := engine.db.QueryContext(t.Context(), statement)
			requirements.NoError(err)
			defer func() { _ = rows.Close() }()
			var got []string
			for rows.Next() {
				var address, domain string
				requirements.NoError(rows.Scan(&address, &domain))
				got = append(got, address+"|"+domain)
			}
			requirements.NoError(rows.Err())
			requirements.NoError(rows.Close())
			assert.ElementsMatch(t, []string{"author@example.test|example.test", "notifications@github.com|github.com"}, got, statement)
		}()
	}
}
