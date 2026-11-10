package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFullSyncRelayedSenders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	env := newTestEnv(t)
	seedMessages(env, 2, 1001, "one", "two")
	raw := map[string][]byte{}
	for id, from := range map[string]string{"one": "Alice <notifications@github.com>\r\nX-GitHub-Sender: Alice", "two": "notifications@github.com\r\nX-GitHub-Sender: Bob"} {
		raw[id] = []byte("From: " + from + "\r\nTo: test@example.com\r\nAuthentication-Results: mx.example.test; dmarc=pass header.from=github.com\r\nSubject: Notice\r\n\r\nHello")
		env.Mock.Messages[id].Raw = raw[id]
	}
	runFullSync(t, env)
	rows, err := env.Store.DB().QueryContext(t.Context(), `SELECT m.id, m.source_message_id, pi.identifier_value, p.display_name, mr.participant_id = m.sender_id FROM messages m JOIN participant_identifiers pi ON pi.participant_id = m.sender_id AND pi.identifier_type = 'github' JOIN participants p ON p.id = m.sender_id JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'`)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	ids := map[string]int64{}
	for rows.Next() {
		var id int64
		var source, login, name string
		var same bool
		requirements.NoError(rows.Scan(&id, &source, &login, &name, &same))
		assertions.True(same)
		got[source] = login + "|" + name
		ids[source] = id
	}
	requirements.NoError(rows.Err())
	requirements.NoError(rows.Close())
	assertions.Equal(map[string]string{"one": "alice|Alice", "two": "bob|bob"}, got)
	for source, id := range ids {
		msg, err := env.Store.GetMessage(id)
		requirements.NoError(err)
		archived, err := env.Store.GetMessageRawContext(t.Context(), id)
		requirements.NoError(err)
		assertions.Equal(raw[source], archived)
		assertions.True(senderauth.Evaluate(archived, []string{"mx.example.test"}, msg.FromEmail).Verified)
	}
}

func TestRelayedSendersIncrementalAndIMAP(t *testing.T) {
	raw := []byte("From: GitHub <notifications@github.com>\r\nTo: test@example.com\r\nX-GitHub-Sender: Alice\r\nSubject: Notice\r\n\r\nHello")
	check := func(t *testing.T, env *TestEnv) {
		t.Helper()
		var id int64
		var login string
		require.NoError(t, env.Store.DB().QueryRowContext(t.Context(), `SELECT m.id, pi.identifier_value FROM messages m JOIN participant_identifiers pi ON pi.participant_id = m.sender_id WHERE pi.identifier_type = 'github'`).Scan(&id, &login))
		assert.Equal(t, "alice", login)
		message, err := env.Store.GetMessage(id)
		require.NoError(t, err)
		assert.Equal(t, "notifications@github.com", message.FromEmail)
	}
	t.Run("gmail incremental", func(t *testing.T) {
		env := newTestEnv(t)
		env.CreateSourceWithHistory(t, "12340")
		env.Mock.AddMessage("one", raw, []string{"INBOX"})
		env.SetHistory(12350, historyAdded("one"))
		runIncrementalSync(t, env)
		check(t, env)
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
		check(t, env)
	})
}

func TestRelayedSenderCustomRuleAndLinkedContact(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	env := newTestEnv(t)
	env.SetOptions(t, func(opts *Options) {
		opts.RelayedSenders = []mime.RelayedSenderRule{{Address: "relay@example.test", Header: "X-Author", IdentifierType: "example"}}
	})
	seedMessages(env, 1, 1001, "one")
	env.Mock.Messages["one"].Raw = []byte("From: Alice Author <relay@example.test>\r\nTo: test@example.com\r\nX-Author: author\r\nSubject: Notice\r\n\r\nHello")
	runFullSync(t, env)
	var id, author int64
	requirements.NoError(env.Store.DB().QueryRowContext(t.Context(), `SELECT id, sender_id FROM messages`).Scan(&id, &author))
	var displayName string
	requirements.NoError(env.Store.DB().QueryRowContext(t.Context(), `SELECT display_name FROM participants WHERE id=?`, author).Scan(&displayName))
	assertions.Equal("Alice Author", displayName)
	var relayNames int
	requirements.NoError(env.Store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM participants WHERE email_address='relay@example.test' AND display_name='Alice Author'`).Scan(&relayNames))
	assertions.Zero(relayNames)
	contact, err := env.Store.EnsureParticipant("author@example.test", "Author", "example.test")
	requirements.NoError(err)
	requirements.NoError(env.Store.MergeParticipants(author, contact))
	msg, err := env.Store.GetMessage(id)
	requirements.NoError(err)
	assertions.Equal("relay@example.test", msg.FromEmail)
	for value, want := range map[string]int64{"relay@example.test": 1, "author@example.test": 0} {
		_, count, err := env.Store.SearchMessagesQueryContext(t.Context(), search.Parse("from:"+value), 0, 10)
		requirements.NoError(err)
		assertions.Equal(want, count, value)
	}
}
