package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFullSyncRelayedSenders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	seedMessages(env, 2, 1001, "one", "two")
	for id, from := range map[string]string{"one": "Alice <notifications@github.com>\r\nX-GitHub-Sender: Alice", "two": "notifications@github.com\r\nX-GitHub-Sender: Bob"} {
		env.Mock.Messages[id].Raw = []byte("From: " + from + "\r\nTo: test@example.com\r\nSubject: Notice\r\n\r\nHello")
	}
	runFullSync(t, env)
	_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "one", SourceID: source.ID})
	requirements.NoError(err)
	rows, err := env.Store.DB().QueryContext(t.Context(), `SELECT m.source_message_id, pi.identifier_value, p.display_name, mr.participant_id = m.sender_id, mr.email_address, mr.is_relayed FROM messages m JOIN participant_identifiers pi ON pi.participant_id = m.sender_id AND pi.identifier_type = 'github' JOIN participants p ON p.id = m.sender_id JOIN message_recipients mr ON mr.message_id = m.id AND mr.recipient_type = 'from'`)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	for rows.Next() {
		var source, login, name, address string
		var same, relayed bool
		requirements.NoError(rows.Scan(&source, &login, &name, &same, &address, &relayed))
		assertions.True(same)
		assertions.Equal("notifications@github.com", address)
		assertions.True(relayed)
		got[source] = login + "|" + name
	}
	requirements.NoError(rows.Err())
	requirements.NoError(rows.Close())
	assertions.Equal(map[string]string{"one": "alice|Alice", "two": "bob|bob"}, got)
}
