package sync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFullSyncRelayedSenders(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 2, 1001, "one", "two")
	for id, login := range map[string]string{"one": "Alice", "two": "Bob"} {
		env.Mock.Messages[id].Raw = []byte(fmt.Sprintf("From: GitHub <notifications@github.com>\r\nTo: test@example.com\r\nX-GitHub-Sender: %s\r\nSubject: Notice\r\n\r\nHello", login))
	}
	runFullSync(t, env)
	rows, err := env.Store.DB().QueryContext(t.Context(), `SELECT m.source_message_id, pi.identifier_value, mr.email_address, mr.participant_id = m.sender_id FROM messages m JOIN participant_identifiers pi ON pi.participant_id = m.sender_id AND pi.identifier_type = 'github' JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from' ORDER BY m.source_message_id`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, login, address string
		var same bool
		require.NoError(t, rows.Scan(&id, &login, &address, &same))
		assert.Equal(t, "notifications@github.com", address)
		assert.True(t, same)
		got[id] = login
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]string{"one": "alice", "two": "bob"}, got)
}

func TestRelayedSenderEnvelopeSearch(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 1, 1001, "one")
	env.Mock.Messages["one"].Raw = []byte("From: GitHub <notifications@github.com>\r\nTo: test@example.com\r\nX-GitHub-Sender: Alice\r\nSubject: Notice\r\n\r\nHello")
	runFullSync(t, env)
	results, count, err := env.Store.SearchMessagesQueryContext(t.Context(), search.Parse("from:notifications@github.com"), 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Len(t, results, 1)
	assert.Equal(t, "notifications@github.com", results[0].FromEmail)
}

func TestRelayedSenderQueryAndReplacement(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSource(t)
	id := seedRepairRow(t, env.Store, source.ID, "one", "thread", "old")
	env.Mock.Messages["one"] = repairRaw("one", "thread", "Notice", "body", nil)
	env.Mock.Messages["one"].Raw = []byte("From: GitHub <notifications@github.com>\r\nTo: test@example.com\r\nX-GitHub-Sender: Alice\r\nSubject: Notice\r\n\r\nHello")
	_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "one", SourceID: source.ID})
	require.NoError(t, err)
	var login string
	require.NoError(t, env.Store.DB().QueryRowContext(t.Context(), `SELECT pi.identifier_value FROM messages m JOIN participant_identifiers pi ON pi.participant_id=m.sender_id WHERE m.id=? AND pi.identifier_type='github'`, id).Scan(&login))
	assert.Equal(t, "alice", login)
	engine := query.NewSQLiteEngine(env.Store.DB())
	rows, err := engine.ListMessages(t.Context(), query.MessageFilter{Sender: "notifications@github.com"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "notifications@github.com", rows[0].FromEmail)
	domains, err := engine.Aggregate(t.Context(), query.ViewDomains, query.AggregateOptions{})
	require.NoError(t, err)
	require.Len(t, domains, 1)
	assert.Equal(t, "github.com", domains[0].Key)
}

func TestRelayedSendersIncrementalAndIMAP(t *testing.T) {
	raw := []byte("From: GitHub <notifications@github.com>\r\nTo: test@example.com\r\nX-GitHub-Sender: Alice\r\nSubject: Notice\r\n\r\nHello")
	t.Run("gmail incremental", func(t *testing.T) {
		env := newTestEnv(t)
		env.CreateSourceWithHistory(t, "12340")
		env.Mock.AddMessage("one", raw, []string{"INBOX"})
		env.SetHistory(12350, historyAdded("one"))
		runIncrementalSync(t, env)
		assertRelayedAuthor(t, env, "alice")
	})
	t.Run("imap", func(t *testing.T) {
		env := newTestEnv(t)
		addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
		testutil.AppendIMAPRawMessage(t, user, "INBOX", raw)
		client := newSyncTestIMAPClient(t, addr)
		t.Cleanup(func() { _ = client.Close() })
		opts := DefaultOptions()
		opts.SourceType = sourceTypeIMAP
		env.Syncer = New(client, env.Store, opts)
		runFullSync(t, env)
		assertRelayedAuthor(t, env, "alice")
	})
}

func assertRelayedAuthor(t *testing.T, env *TestEnv, want string) {
	t.Helper()
	var got string
	require.NoError(t, env.Store.DB().QueryRowContext(t.Context(), `SELECT pi.identifier_value FROM messages m JOIN participant_identifiers pi ON pi.participant_id = m.sender_id WHERE pi.identifier_type = 'github'`).Scan(&got))
	assert.Equal(t, want, got)
}

func TestRelayedSenderCustomRuleAndLinkedContact(t *testing.T) {
	env := newTestEnv(t)
	env.SetOptions(t, func(opts *Options) {
		opts.RelayedSenders = []mime.RelayedSenderRule{{Address: "relay@example.test", Header: "X-Author", IdentifierType: "example"}}
	})
	seedMessages(env, 1, 1001, "one")
	env.Mock.Messages["one"].Raw = []byte("From: Relay <relay@example.test>\r\nTo: test@example.com\r\nX-Author: author\r\nSubject: Notice\r\n\r\nHello")
	runFullSync(t, env)
	var id, author int64
	require.NoError(t, env.Store.DB().QueryRowContext(t.Context(), `SELECT id, sender_id FROM messages`).Scan(&id, &author))
	contact, err := env.Store.EnsureParticipant("author@example.test", "Author", "example.test")
	require.NoError(t, err)
	require.NoError(t, env.Store.MergeParticipants(author, contact))
	msg, err := env.Store.GetMessage(id)
	require.NoError(t, err)
	assert.Equal(t, "relay@example.test", msg.FromEmail)
	var owner int64
	require.NoError(t, env.Store.DB().QueryRowContext(t.Context(), `SELECT participant_id FROM participant_identifiers WHERE identifier_type='example' AND identifier_value='author'`).Scan(&owner))
	assert.Equal(t, contact, owner)
}

func TestRelayedSenderAuthentication(t *testing.T) {
	env := newTestEnv(t)
	seedMessages(env, 1, 1001, "one")
	raw := []byte("From: GitHub <notifications@github.com>\r\nTo: test@example.com\r\nX-GitHub-Sender: Alice\r\nAuthentication-Results: mx.example.test; dmarc=pass header.from=github.com\r\nSubject: Notice\r\n\r\nHello")
	env.Mock.Messages["one"].Raw = raw
	runFullSync(t, env)
	var id int64
	require.NoError(t, env.Store.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&id))
	msg, err := env.Store.GetMessage(id)
	require.NoError(t, err)
	archived, err := env.Store.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, archived)
	verdict := senderauth.Evaluate(archived, []string{"mx.example.test"}, msg.FromEmail)
	assert.True(t, verdict.Verified)
	assert.Equal(t, "github.com", verdict.Domain)
}
