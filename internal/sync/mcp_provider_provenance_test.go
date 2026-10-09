package sync

import (
	"errors"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func enableProviderEvents(t *testing.T, env *TestEnv, sourceType string) {
	t.Helper()
	require := Require.New(t)

	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: sourceType, Kinds: []string{"message"}}}})
	require.NoError(err)
}

func providerEventCount(t *testing.T, env *TestEnv) int {
	t.Helper()
	require := Require.New(t)

	var count int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family = 'msgvault.message_archived'`).Scan(&count))
	return count
}

func TestMCPProviderGmailFullMutedIncrementalLive(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	env := newTestEnv(t)
	enableProviderEvents(t, env, "gmail")
	seedMessages(env, 1, 1000, "historical")
	runFullSync(t, env)
	assert.Zero(providerEventCount(t, env))
	env.Mock.AddMessage("live-delta", testemail.NewMessage().From("sender@example.test").To("receiver@example.test").Body("Live synthetic body").Bytes(), []string{"INBOX"})
	env.SetHistory(1001, historyAdded("live-delta"))
	runIncrementalSync(t, env)
	require.Equal(1, providerEventCount(t, env), "exactly one event is required before reading its archived identity")
	var providerID string
	require.NoError(env.Store.DB().QueryRow(`SELECT source_message_id FROM messages WHERE id = (SELECT message_id FROM mcp_event_log WHERE family = 'msgvault.message_archived')`).Scan(&providerID))
	assert.Equal("live-delta", providerID)
	assertBodyContains(t, env.Store, "live-delta", "Live synthetic body")
}

func TestMCPProviderGmailReplayDebtMutedWithinLiveRun(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")
	recordIncrementalSyncRunItems(t, env, source.ID, store.SyncRunItem{SourceMessageID: "old-fetch-debt", Phase: syncItemPhaseFetch, Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError})
	env.Mock.AddMessage("old-fetch-debt", testemail.NewMessage().Body("Recovered historical body").Bytes(), []string{"INBOX"})
	env.Mock.AddMessage("live-delta", testemail.NewMessage().Body("New live body").Bytes(), []string{"INBOX"})
	env.SetHistory(1001, historyAdded("live-delta"))
	enableProviderEvents(t, env, "gmail")
	runIncrementalSync(t, env)
	assertMessageCount(t, env.Store, 2)
	require.Equal(1, providerEventCount(t, env), "exactly one event is required before reading its archived identity")
	var providerID string
	require.NoError(env.Store.DB().QueryRow(`SELECT source_message_id FROM messages WHERE id = (SELECT message_id FROM mcp_event_log WHERE family = 'msgvault.message_archived')`).Scan(&providerID))
	assert.Equal("live-delta", providerID)
}

func TestMCPProviderGmailLabelChangeRecoveryMuted(t *testing.T) {
	assert := Assert.New(t)

	env := newTestEnv(t)
	enableProviderEvents(t, env, "gmail")
	env.Mock.Profile.MessagesTotal = 2
	env.Mock.Profile.HistoryID = 1000
	env.Mock.MessagePages = [][]string{{"historical", "missed-in-full"}}
	env.Mock.AddMessage("historical", testMIME(), []string{"INBOX"})
	env.Mock.AddMessage("missed-in-full", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["missed-in-full"] = errors.New("temporary fetch failure")
	runFullSync(t, env)
	assertMessageCount(t, env.Store, 1)
	delete(env.Mock.GetMessageError, "missed-in-full")
	env.SetHistory(1001, historyLabelAdded("missed-in-full", "STARRED"))

	runIncrementalSync(t, env)

	assertMessageCount(t, env.Store, 2)
	assert.Zero(providerEventCount(t, env), "a label change on an old message is not a live arrival")
}

func TestMCPProviderIMAPMailboxDeltaLiveRecoveryMuted(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	env := newTestEnv(t)
	enableProviderEvents(t, env, "imap")
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	appendMessage := func(id string) {
		testutil.AppendIMAPRawMessage(t, user, "INBOX", testemail.NewMessage().Header("Message-ID", "<"+id+"@example.test>").Body(id+" body").CRLF().Bytes())
	}
	appendMessage("historical")
	first := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(first.Client, env.Store, opts)
	runFullSync(t, env)
	saved := first.ObservedFolderStates()
	require.NoError(first.Close())
	assert.Zero(providerEventCount(t, env))
	appendMessage("live-delta")
	second := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStates(saved))
	env.Syncer = New(second.Client, env.Store, opts)
	runFullSync(t, env)
	assert.Equal(store.IngestLive, second.MessageIngestContext("INBOX|2").Mode, "saved=%+v deltas=%+v", saved, second.ObservedMailboxDeltas())
	assert.Equal(1, providerEventCount(t, env))
	saved = second.ObservedFolderStates()
	require.NoError(second.Close())
	appendMessage("recovery-found")
	third := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStates(saved), imapclient.WithForceFullEnumeration())
	env.Syncer = New(third.Client, env.Store, opts)
	runFullSync(t, env)
	assert.Equal(1, providerEventCount(t, env), "forced recovery discovers historical rows without live attribution")
	assertMessageCount(t, env.Store, 3)
}
