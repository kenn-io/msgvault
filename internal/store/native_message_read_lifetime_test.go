package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

// A current read is confirmed under native writer fences. The later coherent
// message write must still match that lifetime, even if a scope ID is reused.
func TestNativeMessageConfirmedReadCannotBorrowReplacement(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
	require.NoError(err)
	require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}))
	inventory := store.NativeMessageScopeInventory{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Kind: "gmail_label", Scopes: []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "Inbox"}}}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	pin, found, err := f.Store.LookupNativeMessageScope(t.Context(), f.Source.ID, source.Reference, "gmail_label", "INBOX")
	require.NoError(err)
	require.True(found)
	confirmed, err := f.Store.ConfirmNativeMessageScopeRead(t.Context(), store.NativeMessageScopeRead{SourceID: f.Source.ID, SourceReference: source.Reference, SourceType: "gmail", NativeIdentity: f.Source.Identifier, Scopes: []store.NativeMessageScopeLookup{pin}})
	require.NoError(err)
	require.Len(confirmed, 1)
	_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}, {Family: "msgvault.inbox_message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	inventory.Scopes = nil
	inventory.RetiredProviderIDs = []string{"INBOX"}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	inventory.RetiredProviderIDs = nil
	inventory.Scopes = []store.NativeMessageScope{{ProviderID: "INBOX", DisplayName: "Restored inbox"}}
	require.NoError(f.Store.ObserveNativeMessageScopes(t.Context(), inventory))
	data := &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-stale-native-message").Build(), BodyText: sql.NullString{String: "Synthetic stale native body", Valid: true}, NativeScopes: &store.NativeMessageScopeMembership{SourceReference: source.Reference, Scopes: confirmed}}
	_, err = f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessage(data)
	require.Error(err, "capture must reject the scope replacement after native read confirmation")
	var messages, events int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id='synthetic-stale-native-message'`).Scan(&messages))
	assert.Zero(messages)
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
