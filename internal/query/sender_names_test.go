package query

import (
	"bytes"
	"fmt"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSenderNameHintQueryFailure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	q := search.Parse("from:alex")
	requirements.NoError(env.DB.Close())
	var diagnostic bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostic)
	t.Cleanup(func() { log.SetOutput(previous) })
	assertions.Empty(env.Engine.MatchSenderName(env.Ctx, q))
	assertions.Contains(diagnostic.String(), "sender hint query failed")
}

func TestSenderNameHints(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	for i := range 4 {
		p := env.AddParticipant(dbtest.ParticipantOpts{Email: new(fmt.Sprintf("sender%d@example.com", i))})
		for j := 0; j <= i; j++ {
			id := env.AddMessage(dbtest.MessageOpts{FromID: p, Subject: "hintfixture"})
			// Only the final message supplies a matching name; count every message.
			if j == i {
				env.SetFromName(id, "Alex Kim")
			}
		}
	}
	q := search.Parse("from:alex subject:hintfixture")
	hints := env.Engine.MatchSenderName(env.Ctx, q)
	requirements.Len(hints, 1)
	assertions.Equal(`from: matches addresses; senders matching "alex": try from:sender3@example.com subject:hintfixture (4 messages); other matching senders: sender2@example.com (3 messages), sender1@example.com (2 messages)`, hints[0].Message)
	for _, tc := range []struct {
		q             string
		limit, offset int
		want          bool
	}{
		{"from:alex subject:hintfixture", 10, 0, true},
		{"from:alex subject:hintfixture | from:alex subject:absent", 10, 0, false},
		{"from:alex subject:hintfixture", 0, 0, false},
		{"from:alex subject:hintfixture", 10, 10, false},
		{"from:sender3@example.com subject:hintfixture", 10, 0, false},
		{"from:alex from:kim", 10, 0, false},
		{"from:alex subject:absent", 10, 0, false},
	} {
		t.Run(fmt.Sprintf("%s/%d/%d", tc.q, tc.limit, tc.offset), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			q := search.ParseOR(tc.q)
			original := search.ParseOR(tc.q)
			result, err := env.Engine.SearchFastWithStats(env.Ctx, q, tc.q, MessageFilter{}, ViewNoStats, tc.limit, tc.offset)
			requirements.NoError(err)
			assertions.Equal(tc.want, len(result.Hints) > 0)
			assertions.Equal(original, q)
		})
	}
}

func TestSenderNameHintSpecialFragments(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("mary.jones@example.org"), DisplayName: new("Mary Jo")})
	env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture"})
	other := env.AddParticipant(dbtest.ParticipantOpts{Email: new("chat-handle"), DisplayName: new("Alex_% Kim")})
	env.AddMessage(dbtest.MessageOpts{FromID: other, Subject: "hintfixture"})
	wildcard := env.AddParticipant(dbtest.ParticipantOpts{Email: new("other-handle"), DisplayName: new("Alex Other")})
	env.AddMessage(dbtest.MessageOpts{FromID: wildcard, Subject: "hintfixture"})
	for _, tc := range []struct{ input, message, retry string }{
		{"from:mary.jo subject:hintfixture", `from: matches addresses; senders matching "mary.jo": try from:mary.jones@example.org subject:hintfixture (1 message)`, "from:mary.jones@example.org subject:hintfixture"},
		{"from:@ subject:hintfixture", `from: matches addresses; senders matching "@": try from:mary.jones@example.org subject:hintfixture (1 message)`, "from:mary.jones@example.org subject:hintfixture"},
		{"from:alex_% subject:hintfixture", `from: matches addresses; senders matching "alex_%": try from:chat-handle subject:hintfixture (1 message)`, "from:chat-handle subject:hintfixture"},
	} {
		hints := env.Engine.MatchSenderName(env.Ctx, search.Parse(tc.input))
		requirements.Len(hints, 1)
		assertions.Equal(tc.message, hints[0].Message)
		assertions.Equal(tc.retry, hints[0].RetryWith.Query)
	}
}

func TestSenderNameHintParsedAddressRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	env := newTestEnv(t)
	upper := env.AddParticipant(dbtest.ParticipantOpts{Email: new("École@example.com"), DisplayName: new("Alex Kim")})
	env.AddMessage(dbtest.MessageOpts{FromID: upper, Subject: "hintfixture"})
	q := search.Parse("from:alex")
	hints := env.Engine.MatchSenderName(env.Ctx, q)
	assert.Empty(hints, "an address that the parsed exact retry cannot match must be excluded")

	for range 3 {
		env.AddMessage(dbtest.MessageOpts{FromID: upper, Subject: "hintfixture"})
	}
	lower := env.AddParticipant(dbtest.ParticipantOpts{Email: new("école@example.com"), DisplayName: new("Alex Kim")})
	var wantIDs []int64
	for range 3 {
		wantIDs = append(wantIDs, env.AddMessage(dbtest.MessageOpts{FromID: lower, Subject: "hintfixture"}))
	}
	for _, address := range []string{"first@example.org", "second@example.org", "third@example.org"} {
		sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new(address), DisplayName: new("Alex Kim")})
		env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture"})
	}
	hints = env.Engine.MatchSenderName(env.Ctx, q)
	require.Len(hints, 1)
	assert.Equal(`from: matches addresses; senders matching "alex": try from:école@example.com (3 messages); other matching senders: first@example.org (1 message), second@example.org (1 message)`, hints[0].Message)
	assert.Equal("from:école@example.com", hints[0].RetryWith.Query)
	results, err := env.Engine.Search(env.Ctx, search.Parse(hints[0].RetryWith.Query), 10, 0)
	require.NoError(err)
	var gotIDs []int64
	for _, result := range results {
		gotIDs = append(gotIDs, result.ID)
	}
	assert.ElementsMatch(wantIDs, gotIDs)
}

func TestSenderNameHintKnownAddressScope(t *testing.T) {
	env := newTestEnv(t)
	visible := env.AddSource(dbtest.SourceOpts{Type: "gmail", Identifier: "visible@example.com"})
	hidden := env.AddSource(dbtest.SourceOpts{Type: "gmail", Identifier: "hidden@example.com"})
	conversation := env.AddConversation(dbtest.ConversationOpts{SourceID: visible})
	otherConversation := env.AddConversation(dbtest.ConversationOpts{SourceID: hidden})
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("sal@example.com")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "hintfixture", SourceID: visible, ConversationID: conversation})
	known := env.AddParticipant(dbtest.ParticipantOpts{Email: new("Al@Example.com"), DisplayName: new("Alex")})
	otherID := env.AddMessage(dbtest.MessageOpts{FromID: known, Subject: "hintfixture", SourceID: hidden, ConversationID: otherConversation})
	_, err := env.DB.ExecContext(t.Context(), `UPDATE messages SET account_address = ?, account_path = 'inbound' WHERE id = ?`, "visible@example.com", id)
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(), `UPDATE messages SET account_address = ?, account_path = 'inbound' WHERE id = ?`, "hidden@example.com", otherID)
	require.NoError(t, err)
	nameSender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("visible@example.com"), DisplayName: new("Jordan Lee")})
	nameID := env.AddMessage(dbtest.MessageOpts{FromID: nameSender, Subject: "hintfixture", SourceID: visible, ConversationID: conversation})
	env.SetFromName(nameID, "Alex Kim")
	env.AddMessage(dbtest.MessageOpts{FromID: nameSender, Subject: "hintfixture"})
	overridden := env.AddParticipant(dbtest.ParticipantOpts{Email: new("overridden@example.com"), DisplayName: new("Alex")})
	overriddenID := env.AddMessage(dbtest.MessageOpts{FromID: overridden, Subject: "hintfixture", SourceID: visible, ConversationID: conversation})
	env.SetFromName(overriddenID, "Jordan")
	for _, tc := range []struct {
		name              string
		accounts, granted []int64
		operator          string
		filter            MessageFilter
	}{
		{name: "account", accounts: []int64{visible}},
		{name: "context", granted: []int64{visible}},
		{name: "intersection", accounts: []int64{visible, hidden}, granted: []int64{visible}},
		{name: "attributed", operator: " account:visible@example.com"},
		{name: "received", operator: " received:visible@example.com"},
		{name: "source", filter: MessageFilter{SourceID: &visible}},
		{name: "sender name override", filter: MessageFilter{SourceID: &visible}},
		{name: "sources", filter: MessageFilter{SourceIDs: []int64{visible}}},
		{name: "structured", filter: MessageFilter{AccountScopes: []search.AccountScope{{Addresses: []string{"visible@example.com"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := env.Ctx
			if tc.granted != nil {
				ctx = sourcescope.WithSourceIDs(ctx, tc.granted)
			}
			from, address, wantID := "al@example.com", "sal@example.com", id
			if tc.name == "sender name override" {
				from, address, wantID = "alex", "visible@example.com", nameID
			}
			q := search.Parse("from:" + from + " subject:hintfixture" + tc.operator)
			q.AccountIDs = tc.accounts
			result, err := env.Engine.SearchFastWithStats(ctx, q, "", tc.filter, ViewNoStats, 10, 0)
			require.NoError(err)
			assert.Empty(result.Messages)
			hints := result.Hints
			require.Len(hints, 1)
			wantRetry := "from:" + address + " subject:hintfixture" + tc.operator
			assert.Equal(wantRetry, hints[0].RetryWith.Query)
			assert.Equal(`from: matches addresses; senders matching "`+from+`": try `+wantRetry+" (1 message)", hints[0].Message)
			retry := search.Parse(hints[0].RetryWith.Query)
			retry.AccountIDs = tc.accounts
			results, err := env.Engine.SearchFast(ctx, retry, tc.filter, 10, 0)
			require.NoError(err)
			require.Len(results, 1)
			assert.Equal(wantID, results[0].ID)
		})
	}
}

func TestSenderNameHintDeletionScope(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	p := env.AddParticipant(dbtest.ParticipantOpts{Email: new("special@example.com"), DisplayName: new("Alex Kim")})
	id := env.AddMessage(dbtest.MessageOpts{FromID: p, Subject: "hintfixture"})
	q := search.Parse("from:alex subject:hintfixture")
	env.MarkDeletedByID(id)
	q.DeletionScope = search.DeletionScopeActive
	assertions.Nil(env.Engine.MatchSenderName(env.Ctx, q))
	q.DeletionScope = search.DeletionScopeAny
	hints := env.Engine.MatchSenderName(env.Ctx, q)
	requirements.Len(hints, 1)
	retry := search.Parse(hints[0].RetryWith.Query)
	retry.DeletionScope = q.DeletionScope
	results, err := env.Engine.Search(env.Ctx, retry, 10, 0)
	requirements.NoError(err)
	requirements.Len(results, 1)
	assertions.Equal(id, results[0].ID)
}

func TestDuckDBSenderNameHintDelegatesToSQLite(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	env := newTestEnv(t)
	sender := env.AddParticipant(dbtest.ParticipantOpts{Email: new("alex@example.com"), DisplayName: new("Alex Kim")})
	env.AddMessage(dbtest.MessageOpts{FromID: sender, Subject: "sender hint fixture"})
	engine := &DuckDBEngine{sqliteEngine: env.Engine}
	hints := engine.MatchSenderName(t.Context(), search.Parse("from:alex"))
	require.Len(hints, 1)
	assert.Equal("from:alex@example.com", hints[0].RetryWith.Query)
}

func TestSenderNameHintIndexedParity(t *testing.T) {
	db := metadataSearchDB(t)
	_, err := db.ExecContext(t.Context(), `INSERT INTO participants(id,email_address,display_name) VALUES(2,'alias@example.org','Jordan');
 INSERT INTO message_recipients(message_id,participant_id,recipient_type,display_name) VALUES
 (1,1,'from',NULL),(2,2,'from','ÉCOLE Alias');`)
	require.NoError(t, err)
	indexed := NewSQLiteEngine(db)
	require.True(t, indexed.hasMetadataFTS(t.Context()))
	scan := NewEngineWithDialect(db, metadataScanDialect{})
	for _, tc := range []struct {
		input, message string
	}{
		{"from:école", `from: matches addresses; senders matching "école": try from:alias@example.org (1 message); other matching senders: contact@example.org (1 message)`},
		{"from:éc", `from: matches addresses; senders matching "éc": try from:alias@example.org (1 message); other matching senders: contact@example.org (1 message)`},
		{"from:example.o", `from: matches addresses; senders matching "example.o": try from:alias@example.org (1 message); other matching senders: contact@example.org (1 message)`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			for _, engine := range []*SQLiteEngine{indexed, scan} {
				hints := engine.MatchSenderName(t.Context(), search.Parse(tc.input))
				require.Len(t, hints, 1)
				assert.Equal(t, tc.message, hints[0].Message)
				assert.Equal(t, "from:alias@example.org", hints[0].RetryWith.Query)
			}
		})
	}
}
