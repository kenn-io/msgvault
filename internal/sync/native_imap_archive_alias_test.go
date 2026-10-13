package sync

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"testing"
)

// Legacy archive aliases remain usable. They cannot claim the canonical native
// account route or grant Events merely because the archive contains messages.
func TestNativeIMAPLegacyArchiveAliasStaysUsableWithoutEvents(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	env := newTestEnv(t)
	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "imap", Kinds: []string{"message"}}}})
	require.NoError(err)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	testutil.AppendIMAPRawMessage(t, user, "INBOX", testemail.NewMessage().Header("Message-ID", "<synthetic-alias@example.net>").Body("synthetic archived body").CRLF().Bytes())
	native := newSyncTestIMAPClient(t, addr)
	defer func() { Assert.NoError(t, native.Close()) }()
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	summary, err := New(native.Client, env.Store, opts).Full(t.Context(), "legacy-archive@example.net")
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	source, err := env.Store.GetSourceByTypeAndIdentifier("imap", "legacy-archive@example.net")
	require.NoError(err)
	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
	var count int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Zero(count)
	assertBodyContains(t, env.Store, "INBOX|1", "synthetic archived body")
}
