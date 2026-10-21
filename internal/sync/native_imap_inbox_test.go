package sync

import (
	"context"
	"net"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/gmail"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type countingNativeIMAPInventoryClient struct {
	gmail.API

	identity string
	calls    int
}

func (c *countingNativeIMAPInventoryClient) NativeMailboxSourceIdentity() string  { return c.identity }
func (c *countingNativeIMAPInventoryClient) NativeMailboxInventoryComplete() bool { return false }
func (c *countingNativeIMAPInventoryClient) NativeMailboxInventory(context.Context) (string, []imapclient.NativeMailboxScope, []string, error) {
	c.calls++
	return c.identity, nil, nil, nil
}
func (c *countingNativeIMAPInventoryClient) NativeMailboxSelectedEpochs() map[string]uint32 {
	return nil
}
func (c *countingNativeIMAPInventoryClient) NativeMailboxMemberships(string, []byte) ([]imapclient.NativeMailboxScope, error) {
	return nil, nil
}

func TestNativeIMAPInventoryIsSkippedWithoutSelectedMessageCapture(t *testing.T) {
	require := Require.New(t)

	env := newTestEnv(t)
	source, err := env.Store.GetOrCreateSource(sourceTypeIMAP, "imap://synthetic@example.test")
	require.NoError(err)
	client := &countingNativeIMAPInventoryClient{identity: source.Identifier}
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	syncer := New(client, env.Store, opts)

	require.NoError(syncer.observeNativeIMAPMailboxInventory(t.Context(), source.ID))
	require.Zero(client.calls, "NativeMailboxInventory must not run when Events message capture is not selected")
}

func configureNativeIMAPEventCapture(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: sourceTypeIMAP, Kinds: []string{"message"}},
		{Family: "msgvault.source_message_archived", SourceType: sourceTypeIMAP, Kinds: []string{"message"}},
		{Family: "msgvault.inbox_message_archived", SourceType: sourceTypeIMAP, Kinds: []string{"message"}},
	}})
	Require.NoError(t, err)
}

func TestNativeIMAPInboxInventoryCaptureAndEpoch(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	env := newTestEnv(t)
	configureNativeIMAPEventCapture(t, env.Store)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0, "Work": 0})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	route := (&imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}).Identifier()
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	var saved map[string]imapclient.FolderState
	run := func() {
		client := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStates(saved))
		env.Syncer = New(client.Client, env.Store, opts)
		_, err := env.Syncer.Full(t.Context(), route)
		Require.NoError(t, err)
		saved = client.ObservedFolderStates()
		Require.NoError(t, client.Close())
	}
	run()
	source, err := env.Store.GetSourceByTypeAndIdentifier("imap", route)
	require.NoError(err)
	page := func() map[string]store.MCPResource {
		result, err := env.Store.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: source.ID})
		Require.NoError(t, err)
		out := map[string]store.MCPResource{}
		for _, resource := range result.Resources {
			Assert.Equal(t, "imap_mailbox", resource.ScopeKind)
			out[resource.ProviderID] = resource
		}
		return out
	}
	first := page()
	require.Len(first, 2, "native discovery must include quiet authenticated mailboxes")
	var epoch string
	require.NoError(env.Store.DB().QueryRow(`SELECT native_epoch FROM native_message_scopes WHERE id=?`, first["Work"].ID).Scan(&epoch))
	assert.NotEmpty(epoch, "a mailbox name alone cannot establish its lifetime")
	testutil.AppendIMAPRawMessage(t, user, "Work", testemail.NewMessage().From("sender@example.net").Header("Message-ID", "<synthetic-live@example.net>").Body("synthetic mailbox arrival").CRLF().Bytes())
	run()
	var count int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count)
	var scopeID int64
	require.NoError(env.Store.DB().QueryRow(`SELECT scope_id FROM mcp_event_log WHERE family='msgvault.inbox_message_archived'`).Scan(&scopeID))
	assert.Equal(first["Work"].ID, scopeID)
	var memberships int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM native_message_scope_memberships`).Scan(&memberships))
	assert.Equal(1, memberships)
	assert.Equal(first["Work"].Reference, page()["Work"].Reference)

	require.NoError(user.Delete("Work"))
	require.NoError(user.Create("Work", nil))
	testutil.AppendIMAPRawMessage(t, user, "Work", testemail.NewMessage().Header("Message-ID", "<synthetic-reset-history@example.net>").Body("synthetic reset history").CRLF().Bytes())
	run()
	reset := page()["Work"]
	assert.NotEqual(first["Work"].Reference, reset.Reference, "native UIDVALIDITY reset must retire the old mailbox lifetime")
	var resetEpoch string
	require.NoError(env.Store.DB().QueryRow(`SELECT native_epoch FROM native_message_scopes WHERE id=?`, reset.ID).Scan(&resetEpoch))
	assert.NotEqual(epoch, resetEpoch)
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(3, count, "reset enumeration is historical, never a new live arrival")

	require.NoError(user.Rename("Work", "Renamed", nil))
	run()
	_, err = env.Store.GetMCPResource(t.Context(), "inbox", reset.ID)
	require.Error(err, "rename without stable provider identity cannot alias the old mailbox")
	renamed, exists := page()["Renamed"]
	require.True(exists)
	assert.NotEqual(reset.Reference, renamed.Reference)
}
