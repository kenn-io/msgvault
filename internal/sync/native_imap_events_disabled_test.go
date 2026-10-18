package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestNativeIMAPArchivesMessagesWhenEventsAreDisabled(t *testing.T) {
	env := newTestEnv(t)
	assert.False(t, env.Store.MCPMessageCaptureEnabled(sourceTypeIMAP))

	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	testutil.AppendIMAPRawMessage(t, user, "INBOX", testemail.NewMessage().
		Header("Message-ID", "<synthetic-events-disabled@example.net>").
		Body("synthetic archive body with Events disabled").
		CRLF().Bytes())

	client := newSyncTestIMAPClient(t, addr)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	summary, err := New(client, env.Store, opts).Full(t.Context(), client.NativeMailboxSourceIdentity())
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assertBodyContains(t, env.Store, "INBOX|1", "synthetic archive body with Events disabled")
}
